package engine

import (
	"path/filepath"
	"testing"

	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/nft"
	"github.com/sketchain/nfuse/internal/store"
)

// newTestEngine builds a Controller backed by a fresh temp SQLite store and the
// in-memory fakeMgr, so deletion semantics can be exercised without a live
// kernel (fakeMgr.Apply just records the reconciled snapshot).
func newTestEngine(t *testing.T) (*Controller, *fakeMgr, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "del.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := &fakeMgr{exists: false, sample: nft.Sample{
		Counters: map[model.CounterKey]model.Counter{}, AccountUsed: map[int64]uint64{},
	}}
	ctrl, err := New(st, mgr, Options{Logf: t.Logf})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return ctrl, mgr, st
}

// TestDeleteAccountGuardsPortsWithoutCascade verifies the non-cascade delete
// keeps refusing an account that still owns ports (the original behavior).
func TestDeleteAccountGuardsPortsWithoutCascade(t *testing.T) {
	ctrl, _, st := newTestEngine(t)

	id, err := ctrl.AddAccount("alice", model.TierMonthly, 1, 15)
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := ctrl.AddPort(id, 8080); err != nil {
		t.Fatalf("add port: %v", err)
	}

	if err := ctrl.DeleteAccount(id, false); err == nil {
		t.Fatalf("delete without cascade should be rejected while ports exist")
	}

	// The account and its port must still be present in the store.
	snap, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := snap.Account(id); !ok {
		t.Fatalf("account was removed despite the guard")
	}
	if len(snap.PortsFor(id)) != 1 {
		t.Fatalf("port was removed despite the guard: %+v", snap.Ports)
	}
}

// TestDeleteAccountCascade verifies that a cascading delete atomically removes
// the account together with all of its ports (and their counters) in a single
// reconcile.
func TestDeleteAccountCascade(t *testing.T) {
	ctrl, _, st := newTestEngine(t)

	id, err := ctrl.AddAccount("alice", model.TierMonthly, 1, 15)
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := ctrl.AddPort(id, 8080); err != nil {
		t.Fatalf("add port 8080: %v", err)
	}
	if err := ctrl.AddPort(id, 9090); err != nil {
		t.Fatalf("add port 9090: %v", err)
	}

	// Sanity: two ports and two counters per port are persisted before the delete.
	pre, err := st.Load()
	if err != nil {
		t.Fatalf("load pre: %v", err)
	}
	if len(pre.PortsFor(id)) != 2 {
		t.Fatalf("want 2 ports before delete, got %d", len(pre.PortsFor(id)))
	}
	if len(pre.Counters) != 4 {
		t.Fatalf("want 4 counters before delete, got %d", len(pre.Counters))
	}

	if err := ctrl.DeleteAccount(id, true); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}

	// Account, ports and counters must all be gone.
	post, err := st.Load()
	if err != nil {
		t.Fatalf("load post: %v", err)
	}
	if _, ok := post.Account(id); ok {
		t.Fatalf("account survived cascade delete")
	}
	if len(post.Ports) != 0 {
		t.Fatalf("ports survived cascade delete: %+v", post.Ports)
	}
	if len(post.Counters) != 0 {
		t.Fatalf("counters survived cascade delete: %+v", post.Counters)
	}

	// The engine's own view must be empty too.
	if views, _ := ctrl.View(); len(views) != 0 {
		t.Fatalf("view not empty after cascade delete: %+v", views)
	}
}

// TestDeleteAccountNoPortsWithoutCascade verifies that deleting a portless
// account still works with cascade=false (the common case where the guard is
// vacuously satisfied).
func TestDeleteAccountNoPortsWithoutCascade(t *testing.T) {
	ctrl, _, st := newTestEngine(t)

	id, err := ctrl.AddAccount("solo", model.TierUnlimited, 0, 1)
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := ctrl.DeleteAccount(id, false); err != nil {
		t.Fatalf("delete portless account: %v", err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := snap.Account(id); ok {
		t.Fatalf("portless account survived delete")
	}
}
