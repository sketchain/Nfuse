package engine

import (
	"sync"
	"testing"

	"github.com/sketchain/nfuse/internal/model"
)

// portIDFor returns the id of the stored port whose interval starts at start.
func portIDFor(t *testing.T, ctrl *Controller, accountID int64, start uint16) int64 {
	t.Helper()
	snap := ctrl.snap
	for _, p := range snap.PortsFor(accountID) {
		if p.Start == start {
			return p.ID
		}
	}
	t.Fatalf("no port starting at %d for account %d", start, accountID)
	return 0
}

// TestAddPortRangeAndOverlap covers task 1's interval rules at the engine level:
// non-overlapping and adjacent ranges are accepted, while overlaps — range/range,
// range/single, and cross-account — are rejected.
func TestAddPortRangeAndOverlap(t *testing.T) {
	ctrl, _, _ := newTestEngine(t)

	a1, err := ctrl.AddAccount("a1", model.TierMonthly, 1, 15)
	if err != nil {
		t.Fatalf("add account a1: %v", err)
	}
	a2, err := ctrl.AddAccount("a2", model.TierUnlimited, 0, 1)
	if err != nil {
		t.Fatalf("add account a2: %v", err)
	}

	// A range and an adjacent range are both fine.
	if err := ctrl.AddPort(a1, 60000, 60099); err != nil {
		t.Fatalf("add range 60000-60099: %v", err)
	}
	if err := ctrl.AddPort(a1, 60100, 60199); err != nil {
		t.Fatalf("adjacent range 60100-60199 must be allowed: %v", err)
	}

	// Range/range overlap (same account).
	if err := ctrl.AddPort(a1, 60099, 60150); err == nil {
		t.Fatal("overlapping range 60099-60150 must be rejected")
	}
	// Range/single overlap.
	if err := ctrl.AddPort(a1, 60050, 60050); err == nil {
		t.Fatal("single port 60050 inside an existing range must be rejected")
	}
	// Cross-account overlap.
	if err := ctrl.AddPort(a2, 60150, 60250); err == nil {
		t.Fatal("cross-account overlap must be rejected")
	}
	// A single port outside every range on another account is fine.
	if err := ctrl.AddPort(a2, 8080, 0); err != nil {
		t.Fatalf("single port 8080 (end omitted) must be allowed: %v", err)
	}

	// Malformed intervals.
	if err := ctrl.AddPort(a2, 200, 100); err == nil {
		t.Fatal("reversed bounds must be rejected")
	}
	if err := ctrl.AddPort(a2, 0, 0); err == nil {
		t.Fatal("zero port must be rejected")
	}
}

// TestConcurrentOverlappingAddPortSerializes covers task 2's concurrency
// requirement: two goroutines racing to add overlapping ranges must never both
// succeed. Because the overlap check lives inside the reconcile closure (holding
// c.mu across sample→fold→mutate→apply), the two calls serialize and the loser
// sees the winner's row in c.snap, so exactly one succeeds under any schedule.
func TestConcurrentOverlappingAddPortSerializes(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		ctrl, _, st := newTestEngine(t)
		id, err := ctrl.AddAccount("alice", model.TierMonthly, 1, 15)
		if err != nil {
			t.Fatalf("add account: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var err1, err2 error
		go func() { defer wg.Done(); err1 = ctrl.AddPort(id, 60000, 60099) }()
		go func() { defer wg.Done(); err2 = ctrl.AddPort(id, 60050, 60150) }()
		wg.Wait()

		if err1 == nil && err2 == nil {
			t.Fatalf("iter %d: both overlapping AddPort calls succeeded", iter)
		}
		if err1 != nil && err2 != nil {
			t.Fatalf("iter %d: both overlapping AddPort calls failed (err1=%v err2=%v)", iter, err1, err2)
		}
		snap, err := st.Load()
		if err != nil {
			t.Fatalf("iter %d: load: %v", iter, err)
		}
		if got := len(snap.PortsFor(id)); got != 1 {
			t.Fatalf("iter %d: %d ports persisted, want exactly 1", iter, got)
		}
	}
}

// TestEditPortExcludeSelfAndOverlap covers task 3's validation: an edit may slide
// a range onto its own old extent (self is excluded from the overlap check) but
// must be rejected when it collides with a *different* port, and rejected when the
// target port does not exist.
func TestEditPortExcludeSelfAndOverlap(t *testing.T) {
	ctrl, _, _ := newTestEngine(t)
	id, err := ctrl.AddAccount("alice", model.TierMonthly, 1, 15)
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := ctrl.AddPort(id, 60000, 60099); err != nil {
		t.Fatalf("add A: %v", err)
	}
	if err := ctrl.AddPort(id, 60200, 60299); err != nil {
		t.Fatalf("add B: %v", err)
	}
	pa := portIDFor(t, ctrl, id, 60000)
	pb := portIDFor(t, ctrl, id, 60200)

	// Slide A so its new extent overlaps its own old extent: legal (self excluded).
	if err := ctrl.EditPort(pa, 60001, 60100); err != nil {
		t.Fatalf("self-overlapping move must be allowed: %v", err)
	}
	// Edit A to collide with B: rejected.
	if err := ctrl.EditPort(pa, 60250, 60350); err == nil {
		t.Fatal("editing A to overlap B must be rejected")
	}
	// B is untouched and A kept its earlier successful move.
	if got := portIDFor(t, ctrl, id, 60200); got != pb {
		t.Fatalf("B moved unexpectedly")
	}
	if got := portIDFor(t, ctrl, id, 60001); got != pa {
		t.Fatalf("A's successful move was lost")
	}
	// Editing a non-existent port fails.
	if err := ctrl.EditPort(999999, 1, 2); err == nil {
		t.Fatal("editing a non-existent port must fail")
	}
	// Convert a range to a single port (end omitted → single).
	if err := ctrl.EditPort(pb, 60200, 0); err != nil {
		t.Fatalf("range→single edit: %v", err)
	}
	if p := portIDFor(t, ctrl, id, 60200); p != pb {
		t.Fatal("range→single edit changed the port id")
	}
}

// TestEditPortPreservesCounters covers task 3's metering-continuity requirement:
// counters are named by port id, and an edit keeps the id, so the accumulated
// counter value survives the reconcile rebuild. We seed a counter, edit the
// port's number, and assert the value is still seeded into the rebuilt ruleset
// (and still present in SQLite) under the same port id.
func TestEditPortPreservesCounters(t *testing.T) {
	ctrl, mgr, st := newTestEngine(t)
	id, err := ctrl.AddAccount("alice", model.TierMonthly, 1, 15)
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := ctrl.AddPort(id, 8080, 8080); err != nil {
		t.Fatalf("add port: %v", err)
	}
	pid := portIDFor(t, ctrl, id, 8080)

	// Seed accumulated traffic on the port's ingress counter.
	const seededBytes = 5000
	inKey := model.CounterKey{PortID: pid, Dir: model.DirIn}
	if err := st.PersistUsage(nil, map[model.CounterKey]model.Counter{
		inKey: {PortID: pid, Dir: model.DirIn, Packets: 5, Bytes: seededBytes},
	}); err != nil {
		t.Fatalf("seed counter: %v", err)
	}

	// Renumber the port; its id is unchanged.
	if err := ctrl.EditPort(pid, 9090, 9090); err != nil {
		t.Fatalf("edit port: %v", err)
	}

	// The rebuilt ruleset must have reseeded the counter (same id) to its value.
	applied := mgr.lastApplied()
	if got := applied.Counters[inKey].Bytes; got != seededBytes {
		t.Fatalf("counter after edit = %d bytes, want %d (history must survive an edit)", got, seededBytes)
	}
	// And the port now reflects the new number under the same id.
	if p := portIDFor(t, ctrl, id, 9090); p != pid {
		t.Fatalf("edited port id changed: got %d, want %d", p, pid)
	}
	// SQLite still carries the counter under the unchanged id.
	snap, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := snap.Counters[inKey].Bytes; got != seededBytes {
		t.Fatalf("persisted counter after edit = %d bytes, want %d", got, seededBytes)
	}
}
