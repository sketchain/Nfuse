package nft

import (
	"strings"
	"testing"

	"github.com/sketchain/nfuse/internal/model"
)

func TestBuildScript(t *testing.T) {
	m := &execManager{table: "nfuse", iface: "ens5", priority: -500}
	snap := model.Snapshot{
		Accounts: []model.Account{
			{ID: 1, Name: "quota-acct", Tier: model.TierMonthly, LimitGiB: 1, UsedBytes: 100},
			{ID: 2, Name: "unlimited-acct", Tier: model.TierUnlimited},
		},
		Ports: []model.Port{
			{ID: 10, AccountID: 1, Port: 8080},
			{ID: 11, AccountID: 2, Port: 9090},
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
