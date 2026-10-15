package nft

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sketchain/nfuse/internal/model"
)

// TestNftCommandPinsLocale asserts every nft invocation is built with LC_ALL=C so
// nft's strerror()-derived stderr stays English. tableAbsentErr matches those
// English strings to classify a cold start; on a localized host (e.g.
// LANG=zh_CN.UTF-8) an un-pinned locale would break that match.
func TestNftCommandPinsLocale(t *testing.T) {
	cmd := nftCommand(context.Background(), "list", "table", "netdev", "nfuse")

	var found bool
	for _, kv := range cmd.Env {
		if kv == "LC_ALL=C" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("nftCommand env missing LC_ALL=C: %v", cmd.Env)
	}
}

func TestBuildScript(t *testing.T) {
	m := &execManager{table: "nfuse", iface: "ens5", priority: -500}
	snap := model.Snapshot{
		Accounts: []model.Account{
			{ID: 1, Name: "quota-acct", Tier: model.TierMonthly, LimitGiB: 1, UsedBytes: 100},
			{ID: 2, Name: "unlimited-acct", Tier: model.TierUnlimited},
		},
		Ports: []model.Port{
			{ID: 10, AccountID: 1, Start: 8080, End: 8080},
			{ID: 11, AccountID: 2, Start: 9090, End: 9090},
			{ID: 12, AccountID: 1, Start: 60000, End: 60099},
		},
		Counters: map[model.CounterKey]model.Counter{
			{PortID: 10, Dir: model.DirIn}: {PortID: 10, Dir: model.DirIn, Packets: 3, Bytes: 300},
		},
	}
	script := m.buildScript(snap)

	mustContain := []string{
		"add table netdev nfuse",
		"delete table netdev nfuse",
		// quota only for tier a/b, seeded with used bytes
		"add quota netdev nfuse acct1 { over 1073741824 bytes used 100 bytes }",
		// counter seeded with persisted values
		"add counter netdev nfuse p10_in { packets 3 bytes 300 }",
		// device-bound chains with hooks
		`add chain netdev nfuse ingress { type filter hook ingress device "ens5" priority -500; }`,
		`add chain netdev nfuse egress { type filter hook egress device "ens5" priority -500; }`,
		// breaker before counter for the quota account (ingress=dport, egress=sport)
		`add rule netdev nfuse ingress meta l4proto { tcp, udp } th dport 8080 quota name "acct1" drop`,
		`add rule netdev nfuse egress meta l4proto { tcp, udp } th sport 8080 quota name "acct1" drop`,
		`add rule netdev nfuse ingress meta l4proto { tcp, udp } th dport 8080 counter name "p10_in"`,
		// a port range renders as native nft range matches (one rule per direction,
		// sharing the account quota) with a single counter pair for the whole span
		`add rule netdev nfuse ingress meta l4proto { tcp, udp } th dport 60000-60099 quota name "acct1" drop`,
		`add rule netdev nfuse egress meta l4proto { tcp, udp } th sport 60000-60099 counter name "p12_out"`,
	}
	for _, w := range mustContain {
		if !strings.Contains(script, w) {
			t.Errorf("script missing %q\n---\n%s", w, script)
		}
	}

	// The unlimited account must have counters but NO quota rule.
	if strings.Contains(script, "acct2") {
		t.Errorf("unlimited account must not have a quota\n%s", script)
	}
	if !strings.Contains(script, `add rule netdev nfuse ingress meta l4proto { tcp, udp } th dport 9090 counter name "p11_in"`) {
		t.Errorf("unlimited account must still have counter rules\n%s", script)
	}
	if strings.Contains(script, `dport 9090 quota`) {
		t.Errorf("unlimited port must not reference a quota\n%s", script)
	}

	// The breaker rule must appear before the counter rule for a port so that a
	// drop terminates evaluation before the counter advances.
	dropIdx := strings.Index(script, `th dport 8080 quota name "acct1" drop`)
	cntIdx := strings.Index(script, `th dport 8080 counter name "p10_in"`)
	if dropIdx < 0 || cntIdx < 0 || dropIdx > cntIdx {
		t.Errorf("breaker rule must precede counter rule (drop=%d counter=%d)", dropIdx, cntIdx)
	}
}

// TestBuildScriptExpiredAccount: once Snapshot.Now reaches an account's expiry,
// its ports get a bare drop per direction and no quota/counter rules, while the
// quota and counter objects stay declared so usage survives a later renewal.
func TestBuildScriptExpiredAccount(t *testing.T) {
	m := &execManager{table: "nfuse", iface: "ens5", priority: -500}
	deadline := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	snap := model.Snapshot{
		Accounts: []model.Account{
			{ID: 1, Name: "expiring", Tier: model.TierOneShot, LimitGiB: 1, UsedBytes: 100, ExpiresUnix: deadline.Unix()},
		},
		Ports: []model.Port{{ID: 10, AccountID: 1, Start: 8080, End: 8080}},
	}

	snap.Now = deadline.Add(-time.Second)
	live := m.buildScript(snap)
	if !strings.Contains(live, `th dport 8080 quota name "acct1" drop`) {
		t.Errorf("before the deadline the account must be metered normally\n%s", live)
	}

	snap.Now = deadline
	script := m.buildScript(snap)
	for _, w := range []string{
		"add rule netdev nfuse ingress meta l4proto { tcp, udp } th dport 8080 drop\n",
		"add rule netdev nfuse egress meta l4proto { tcp, udp } th sport 8080 drop\n",
		"add quota netdev nfuse acct1 { over 1073741824 bytes used 100 bytes }",
		"add counter netdev nfuse p10_in",
	} {
		if !strings.Contains(script, w) {
			t.Errorf("expired script missing %q\n%s", w, script)
		}
	}
	if strings.Contains(script, `quota name "acct1"`) || strings.Contains(script, `counter name "p10_`) {
		t.Errorf("expired account must not keep quota/counter rules\n%s", script)
	}
}

// TestClassifyTableExists covers the three branches of TableExists's error
// handling (P0-3): a genuine "table absent" cold start, some other non-zero exit
// (e.g. permission denied) which must surface as an error, and a non-exit
// failure (binary missing) which must also surface — never misread as cold.
func TestClassifyTableExists(t *testing.T) {
	// A real *exec.ExitError from a command that exits non-zero.
	exitErr := exec.Command("false").Run()
	if exitErr == nil {
		t.Fatal("expected `false` to exit non-zero")
	}

	// Table absent: non-zero exit whose stderr says so -> cold start.
	for _, stderr := range []string{
		"Error: No such file or directory",
		"Error: table 'nfuse' does not exist",
	} {
		exists, err := classifyTableExists(exitErr, stderr)
		if exists || err != nil {
			t.Errorf("classifyTableExists(_, %q) = (%v, %v), want (false, nil)", stderr, exists, err)
		}
	}

	// Other non-zero exit (e.g. no CAP_NET_ADMIN) -> must be an error.
	if _, err := classifyTableExists(exitErr, "Error: Operation not permitted"); err == nil {
		t.Error("permission-denied exit must return an error, not a cold start")
	}

	// Non-exit failure (binary missing / spawn error) -> must be an error.
	if _, err := classifyTableExists(exec.ErrNotFound, ""); err == nil {
		t.Error("non-exit failure must return an error, not a cold start")
	}
}

func TestNameRoundTrip(t *testing.T) {
	key := model.CounterKey{PortID: 42, Dir: model.DirOut}
	name := counterName(key.PortID, key.Dir)
	got, ok := parseCounterName(name)
	if !ok || got != key {
		t.Errorf("counter round-trip: %q -> %+v, %v", name, got, ok)
	}
	qn := quotaName(7)
	id, ok := parseQuotaName(qn)
	if !ok || id != 7 {
		t.Errorf("quota round-trip: %q -> %d, %v", qn, id, ok)
	}
	if _, ok := parseCounterName("notours"); ok {
		t.Error("should reject foreign counter names")
	}
}
