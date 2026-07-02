// Package nft manages the kernel-side metering and circuit-breaking state via
// nftables.
//
// Backend choice: this package shells out to the `nft(8)` binary rather than
// speaking netlink directly (e.g. google/nftables). Rationale:
//
//   - The whole ruleset is applied from a single `nft -f` script, which nft
//     executes as one atomic transaction. That gives us race-free full
//     reconciliation (flush + rebuild) for free, without hand-rolling netlink
//     batch messages.
//   - `nft -j list` returns structured JSON, so sampling counter/quota values
//     is a simple, stable parse — no manual attribute decoding.
//   - It keeps the dependency surface tiny and matches how an operator would
//     inspect/verify the ruleset by hand.
//
// The trade-off (spawning a process per apply/sample) is irrelevant here: applies
// happen only on mutation, and sampling runs at human timescales (seconds).
//
// ── Ruleset shape ────────────────────────────────────────────────────────────
// A single netdev table holds everything, with two device-bound chains:
//
//	table netdev nfuse {
//	    counter p<portID>_in  { packets .. bytes .. }   # seeded from SQLite
//	    counter p<portID>_out { packets .. bytes .. }
//	    quota   acct<id> { over <limit> bytes used <used> bytes }  # tier a/b only
//
//	    chain ingress { type filter hook ingress device "<iface>" priority -500;
//	        # per port P owned by a quota account, breaker first:
//	        meta l4proto {tcp,udp} th dport P quota name "acct<id>" drop
//	        # then the always-on detail counter (skipped once dropped):
//	        meta l4proto {tcp,udp} th dport P counter name "p<portID>_in"
//	    }
//	    chain egress  { type filter hook egress  device "<iface>" priority -500;
//	        meta l4proto {tcp,udp} th sport P quota name "acct<id>" drop
//	        meta l4proto {tcp,udp} th sport P counter name "p<portID>_out"
//	    }
//	}
//
// Direction rule: ingress matches destination port (traffic *to* the port),
// egress matches source port (traffic *from* the port). They must not be mixed.
//
// Circuit breaking: all ports of one account reference the *same* named quota,
// so the kernel sums in+out bytes with equal weight into one budget. The quota
// statement is evaluated before the counter; when the budget is exceeded it
// yields the `drop` verdict, which terminates the rule so the following counter
// rule is never reached — i.e. the counter sits *after* the drop and stops
// advancing the moment the breaker trips. Unlimited (tier c) accounts get no
// quota rule and thus never break.
package nft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/sketchain/nfuse/internal/model"
)

// Manager owns the kernel-side nftables state for one managed interface.
type Manager interface {
	// Apply reconciles the entire ruleset to match snap. Counters and quotas
	// are seeded from the snapshot so no accounting is lost across a rebuild.
	Apply(snap model.Snapshot) error
	// Sample reads live counter and quota-used values from the kernel.
	Sample() (Sample, error)
	// ResetQuota zeroes one account's named quota in the kernel.
	ResetQuota(accountID int64) error
	// TableExists reports whether the managed netdev table is currently present
	// in the kernel. It distinguishes a cold start (table absent, machine
	// rebooted) from a hot restart (table still live with fresher-than-SQLite
	// accounting), so the engine can pick the authoritative data source.
	TableExists() (bool, error)
	// Teardown removes the managed table entirely.
	Teardown() error
}

// Sample is one live reading of the kernel state.
type Sample struct {
	// Counters keyed by (PortID, Dir).
	Counters map[model.CounterKey]model.Counter
	// AccountUsed maps account id -> quota "used" bytes (present only for
	// accounts that currently have a quota object).
	AccountUsed map[int64]uint64
	At          time.Time
}

// execManager is the production Manager backed by the `nft` binary.
type execManager struct {
	table string // table name, e.g. "nfuse"
	iface string // managed NIC, e.g. "ens5"
	// priority for the netdev hooks; negative runs early.
	priority int
}

// New returns a Manager that drives nftables through the nft binary for the
// given table name and interface.
func New(table, iface string) Manager {
	return &execManager{table: table, iface: iface, priority: -500}
}

// nftCommand builds an `nft` invocation with the process environment plus a
// pinned C locale. tableAbsentErr classifies the cold-start "table absent" case
// by matching nft's English stderr (e.g. "No such file or directory"), but that
// text comes from strerror() and is localized by LANG/LC_* — on a non-English
// host (e.g. LANG=zh_CN.UTF-8) the match would fail and a cold start would be
// misread as an unknown error, so the daemon would refuse to start. Forcing
// LC_ALL=C keeps every nft command's stderr in English. Routing all invocations
// through this one helper guarantees no call site is missed.
func nftCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "nft", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd
}

// Object naming helpers keep kernel identifiers derivable from ids.
func counterName(portID int64, dir model.Direction) string {
	return fmt.Sprintf("p%d_%s", portID, dir)
}
func quotaName(accountID int64) string { return fmt.Sprintf("acct%d", accountID) }

// portMatch returns the nft expression matching a port in the given direction.
func portMatch(dir model.Direction, port uint16) string {
	field := "dport"
	if dir == model.DirOut {
		field = "sport"
	}
	return fmt.Sprintf("meta l4proto { tcp, udp } th %s %d", field, port)
}

// buildScript renders snap into a full `nft -f` script. The add/delete/add
// table idiom makes the whole reconcile a single atomic replace.
func (m *execManager) buildScript(snap model.Snapshot) string {
	var b strings.Builder
	tbl := fmt.Sprintf("netdev %s", m.table)

	// Atomically replace the table.
	fmt.Fprintf(&b, "add table %s\n", tbl)
	fmt.Fprintf(&b, "delete table %s\n", tbl)
	fmt.Fprintf(&b, "add table %s\n", tbl)

	// Quotas (tier a/b), seeded with persisted used bytes.
	acctByID := map[int64]model.Account{}
	for _, a := range snap.Accounts {
		acctByID[a.ID] = a
		if a.Tier.HasQuota() {
			fmt.Fprintf(&b, "add quota %s %s { over %d bytes used %d bytes }\n",
				tbl, quotaName(a.ID), a.LimitBytes(), a.UsedBytes)
		}
	}

	// Counters, seeded with persisted values. Sort ports for stable output.
	ports := append([]model.Port(nil), snap.Ports...)
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	for _, p := range ports {
		for _, dir := range []model.Direction{model.DirIn, model.DirOut} {
			c := snap.Counters[model.CounterKey{PortID: p.ID, Dir: dir}]
			fmt.Fprintf(&b, "add counter %s %s { packets %d bytes %d }\n",
				tbl, counterName(p.ID, dir), c.Packets, c.Bytes)
		}
	}

	// Device-bound chains.
	fmt.Fprintf(&b, "add chain %s ingress { type filter hook ingress device \"%s\" priority %d; }\n",
		tbl, m.iface, m.priority)
	fmt.Fprintf(&b, "add chain %s egress { type filter hook egress device \"%s\" priority %d; }\n",
		tbl, m.iface, m.priority)

	// Rules: breaker (if the account has a quota) then counter, per direction.
	for _, p := range ports {
		acct := acctByID[p.AccountID]
		for _, dir := range []model.Direction{model.DirIn, model.DirOut} {
			chain := "ingress"
			if dir == model.DirOut {
				chain = "egress"
			}
			match := portMatch(dir, p.Port)
			if acct.Tier.HasQuota() {
				fmt.Fprintf(&b, "add rule %s %s %s quota name \"%s\" drop\n",
					tbl, chain, match, quotaName(acct.ID))
			}
			fmt.Fprintf(&b, "add rule %s %s %s counter name \"%s\"\n",
				tbl, chain, match, counterName(p.ID, dir))
		}
	}
	return b.String()
}

func (m *execManager) Apply(snap model.Snapshot) error {
	if err := snap.Validate(); err != nil {
		return err
	}
	script := m.buildScript(snap)
	return m.runScript(script)
}

func (m *execManager) runScript(script string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := nftCommand(ctx, "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft -f failed: %v: %s\n--- script ---\n%s", err, strings.TrimSpace(stderr.String()), script)
	}
	return nil
}

func (m *execManager) ResetQuota(accountID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := nftCommand(ctx, "reset", "quota", "netdev", m.table, quotaName(accountID))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reset quota %s: %v: %s", quotaName(accountID), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (m *execManager) TableExists() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := nftCommand(ctx, "list", "table", "netdev", m.table)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return classifyTableExists(err, stderr.String())
	}
	return true, nil
}

// classifyTableExists interprets a failed `nft list table` invocation. Only a
// non-zero exit whose stderr explicitly says the table is absent counts as a
// cold start ((false, nil)). Every other failure — permission denied, a
// transient netlink error, a missing binary, a timeout — is returned as an
// error so the daemon refuses to start instead of misreading it as a cold start
// and rolling the quota back to a stale SQLite seed (which could let a breached
// account slip back under budget).
func classifyTableExists(runErr error, stderr string) (bool, error) {
	var ee *exec.ExitError
	if errors.As(runErr, &ee) && tableAbsentErr(stderr) {
		return false, nil
	}
	return false, fmt.Errorf("nft list table: %v: %s", runErr, strings.TrimSpace(stderr))
}

// tableAbsentErr reports whether nft's stderr unambiguously indicates the table
// does not exist (as opposed to some other non-zero-exit failure).
func tableAbsentErr(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "does not exist")
}

func (m *execManager) Teardown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Ignore "no such table" style errors: teardown is best-effort.
	cmd := nftCommand(ctx, "delete", "table", "netdev", m.table)
	_ = cmd.Run()
	return nil
}

// nftList mirrors the subset of `nft -j list table ...` output we consume.
type nftList struct {
	Nftables []struct {
		Counter *struct {
			Name    string `json:"name"`
			Packets uint64 `json:"packets"`
			Bytes   uint64 `json:"bytes"`
		} `json:"counter"`
		Quota *struct {
			Name string `json:"name"`
			Used uint64 `json:"used"`
		} `json:"quota"`
	} `json:"nftables"`
}

func (m *execManager) Sample() (Sample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := nftCommand(ctx, "-j", "list", "table", "netdev", m.table)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Sample{}, fmt.Errorf("nft list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var parsed nftList
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return Sample{}, fmt.Errorf("parse nft json: %w", err)
	}
	s := Sample{
		Counters:    map[model.CounterKey]model.Counter{},
		AccountUsed: map[int64]uint64{},
		At:          time.Now(),
	}
	for _, item := range parsed.Nftables {
		if c := item.Counter; c != nil {
			if key, ok := parseCounterName(c.Name); ok {
				s.Counters[key] = model.Counter{PortID: key.PortID, Dir: key.Dir, Packets: c.Packets, Bytes: c.Bytes}
			}
		}
		if q := item.Quota; q != nil {
			if id, ok := parseQuotaName(q.Name); ok {
				s.AccountUsed[id] = q.Used
			}
		}
	}
	return s, nil
}

// parseCounterName inverts counterName: "p<id>_in" / "p<id>_out".
func parseCounterName(name string) (model.CounterKey, bool) {
	if !strings.HasPrefix(name, "p") {
		return model.CounterKey{}, false
	}
	rest := name[1:]
	var dir model.Direction
	switch {
	case strings.HasSuffix(rest, "_in"):
		dir = model.DirIn
		rest = strings.TrimSuffix(rest, "_in")
	case strings.HasSuffix(rest, "_out"):
		dir = model.DirOut
		rest = strings.TrimSuffix(rest, "_out")
	default:
		return model.CounterKey{}, false
	}
	var id int64
	if _, err := fmt.Sscanf(rest, "%d", &id); err != nil {
		return model.CounterKey{}, false
	}
	return model.CounterKey{PortID: id, Dir: dir}, true
}

// parseQuotaName inverts quotaName: "acct<id>".
func parseQuotaName(name string) (int64, bool) {
	if !strings.HasPrefix(name, "acct") {
		return 0, false
	}
	var id int64
	if _, err := fmt.Sscanf(name[len("acct"):], "%d", &id); err != nil {
		return 0, false
	}
	return id, true
}
