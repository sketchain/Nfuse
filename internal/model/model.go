// Package model defines the core domain types for Nfuse: the tree of
// accounts -> ports -> counters, together with the tiering / reset policy.
//
// The model is deliberately transport-agnostic about how metering and circuit
// breaking are enforced in the kernel; it only describes *what* the desired
// state is. The nft package translates a model.Snapshot into an nftables
// ruleset, and the store package persists it.
package model

import (
	"fmt"
	"strconv"
	"strings"
)

// Tier is the account policy tier.
//
//	TierMonthly (a): quota of LimitBytes, reset every billing cycle.
//	TierOneShot (b): quota of LimitBytes, never reset (permanent breaker).
//	TierUnlimited (c): no quota, counters only, never breaks.
//
// Kernel-side, TierMonthly and TierOneShot are identical ("drop once over
// LimitBytes"); the only difference is whether user space performs a periodic
// reset. TierUnlimited installs no quota object at all.
//
// Switching tiers is intentionally non-destructive to UsedBytes: moving an
// account from a/b to c only *pauses* billing (the quota object goes away), and
// moving it back revives the historical usage and re-seeds it into the kernel
// quota. This is deliberate — it is not a bug to "fix". To actually zero usage,
// reset the account explicitly (see ResetAccount).
type Tier string

const (
	TierMonthly   Tier = "a"
	TierOneShot   Tier = "b"
	TierUnlimited Tier = "c"
)

// HasQuota reports whether the tier is enforced by a kernel quota object.
func (t Tier) HasQuota() bool { return t == TierMonthly || t == TierOneShot }

// Resets reports whether user space periodically resets the quota.
func (t Tier) Resets() bool { return t == TierMonthly }

// Valid reports whether t is one of the known tiers.
func (t Tier) Valid() bool {
	switch t {
	case TierMonthly, TierOneShot, TierUnlimited:
		return true
	}
	return false
}

// Describe returns a short human label for the tier.
func (t Tier) Describe() string {
	switch t {
	case TierMonthly:
		return "monthly"
	case TierOneShot:
		return "one-shot"
	case TierUnlimited:
		return "unlimited"
	default:
		return "unknown"
	}
}

// Direction is a per-port traffic direction.
//
//	DirIn  : traffic arriving at the managed NIC destined to the port
//	         (netdev ingress hook, matched on destination port).
//	DirOut : traffic leaving the managed NIC sourced from the port
//	         (netdev egress hook, matched on source port).
type Direction string

const (
	DirIn  Direction = "in"
	DirOut Direction = "out"
)

// Account is the top of the tree: an identity that owns zero or more ports and
// carries a quota/reset policy. UsedBytes is the persisted snapshot of the
// kernel quota's consumed value (the authoritative accumulated usage that must
// survive reboots).
type Account struct {
	ID       int64
	Name     string
	Tier     Tier
	LimitGiB float64 // quota limit in GiB; ignored for TierUnlimited
	// BillingAnchorDay is the day-of-month (1-28) on which a TierMonthly
	// account's cycle rolls over. Restricted to 1-28 to avoid month-length gaps;
	// out-of-range values are rejected at the mutation layer, not clamped.
	BillingAnchorDay int
	UsedBytes        uint64 // persisted quota "used" snapshot
	LastResetUnix    int64  // unix seconds of last monthly reset
}

// LimitBytes returns the quota ceiling in bytes (0 for unlimited tiers).
func (a Account) LimitBytes() uint64 {
	if !a.Tier.HasQuota() {
		return 0
	}
	return uint64(a.LimitGiB * float64(1<<30))
}

// Breached reports whether the account's persisted usage is at/over its limit.
// This is a display convenience only; the actual breaker lives in the kernel.
func (a Account) Breached() bool {
	if !a.Tier.HasQuota() {
		return false
	}
	return a.UsedBytes >= a.LimitBytes()
}

// Port is a metered port (or contiguous port range) belonging to exactly one
// account. The range is the closed interval [Start, End]; a single port is the
// degenerate case Start == End. A range is metered and circuit-broken as one
// whole (one counter pair, one shared quota), not per-port within the range.
type Port struct {
	ID        int64
	AccountID int64
	Start     uint16
	End       uint16
}

// Overlaps reports whether the two closed intervals share any port number.
// Adjacent ranges (e.g. 60000-60099 and 60100-60199) do not overlap.
func (p Port) Overlaps(q Port) bool {
	return p.Start <= q.End && q.Start <= p.End
}

// String renders the port for display: a bare number for a single port, or
// "start-end" for a range.
func (p Port) String() string {
	if p.Start == p.End {
		return strconv.Itoa(int(p.Start))
	}
	return fmt.Sprintf("%d-%d", p.Start, p.End)
}

// ValidRange reports whether the interval is well-formed: 1 ≤ Start ≤ End.
// (End is a uint16 so it can never exceed 65535.)
func (p Port) ValidRange() bool {
	return p.Start >= 1 && p.Start <= p.End
}

// Counter is the persisted snapshot of one per-port, per-direction nft counter.
type Counter struct {
	PortID  int64
	Dir     Direction
	Packets uint64
	Bytes   uint64
}

// Snapshot is a full, self-consistent view of desired state. It is the unit the
// nft package renders into a ruleset and the store package loads/saves.
type Snapshot struct {
	Accounts []Account
	Ports    []Port
	Counters map[CounterKey]Counter // keyed by (PortID, Dir)
}

// CounterKey identifies a counter within a Snapshot.
type CounterKey struct {
	PortID int64
	Dir    Direction
}

// PortsFor returns the ports owned by the given account id.
func (s Snapshot) PortsFor(accountID int64) []Port {
	var out []Port
	for _, p := range s.Ports {
		if p.AccountID == accountID {
			out = append(out, p)
		}
	}
	return out
}

// Account returns the account with the given id, or false.
func (s Snapshot) Account(id int64) (Account, bool) {
	for _, a := range s.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// Validate checks the structural invariants of the model:
//   - every port references an existing account
//   - every port interval is well-formed (1 ≤ Start ≤ End)
//   - no two port intervals overlap, across the whole managed set and across
//     accounts (a port belongs to exactly one account; adjacent ranges are fine)
//   - accounts with a quota tier have a positive limit
//
// This is the last-line structural guard applied before every kernel Apply; the
// engine additionally enforces overlap atomically inside its reconcile path.
func (s Snapshot) Validate() error {
	accts := map[int64]bool{}
	for _, a := range s.Accounts {
		if !a.Tier.Valid() {
			return fmt.Errorf("account %q: invalid tier %q", a.Name, a.Tier)
		}
		if a.Tier.HasQuota() && a.LimitGiB <= 0 {
			return fmt.Errorf("account %q: tier %s requires a positive limit", a.Name, a.Tier)
		}
		accts[a.ID] = true
	}
	for i, p := range s.Ports {
		if !accts[p.AccountID] {
			return fmt.Errorf("port %s references unknown account %d", p, p.AccountID)
		}
		if !p.ValidRange() {
			return fmt.Errorf("port range %s is invalid (need 1 ≤ start ≤ end)", p)
		}
		for j := 0; j < i; j++ {
			if q := s.Ports[j]; p.Overlaps(q) {
				return fmt.Errorf("port range %s (account %d) overlaps %s (account %d)",
					p, p.AccountID, q, q.AccountID)
			}
		}
	}
	return nil
}

// FormatBytes renders a byte count in human-friendly binary units.
func FormatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// NormalizeName trims and validates an account name for use in nft object ids.
func NormalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name must not be empty")
	}
	return name, nil
}
