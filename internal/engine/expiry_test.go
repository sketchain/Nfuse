package engine

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/store"
)

func viewOf(t *testing.T, c *Controller, id int64) AccountView {
	t.Helper()
	views, _ := c.View()
	for _, v := range views {
		if v.Account.ID == id {
			return v
		}
	}
	t.Fatalf("account %d not in view", id)
	return AccountView{}
}

// TestSetExpiryPastDisablesImmediately: an expiry already in the past takes
// effect in the very reconcile that sets it; clearing it re-enables the account.
func TestSetExpiryPastDisablesImmediately(t *testing.T) {
	ctrl, mgr, st := newTestEngine(t)
	id, err := ctrl.AddAccount("alice", model.TierOneShot, 1, 1)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	past := time.Now().Add(-time.Hour).Unix()
	if err := ctrl.SetExpiry(id, past); err != nil {
		t.Fatalf("set expiry: %v", err)
	}
	applied := mgr.lastApplied()
	acct, _ := applied.Account(id)
	if acct.ExpiresUnix != past || !acct.ExpiredAt(applied.Now) {
		t.Fatalf("applied snapshot should render the account expired: %+v at %v", acct, applied.Now)
	}
	if !viewOf(t, ctrl, id).Expired {
		t.Fatal("view should report the account expired")
	}
	snap, _ := st.Load()
	if a, _ := snap.Account(id); a.ExpiresUnix != past {
		t.Fatalf("expiry not persisted: %d", a.ExpiresUnix)
	}

	if err := ctrl.SetExpiry(id, 0); err != nil {
		t.Fatalf("clear expiry: %v", err)
	}
	if viewOf(t, ctrl, id).Expired {
		t.Fatal("clearing the expiry should re-enable the account")
	}
}

// TestExpiryLoopAppliesWhenDeadlinePasses: a future expiry leaves the account
// live; once the wall clock passes it, syncKernel reconciles and the kernel
// is handed the expired rendering. A second check with nothing changed is a no-op.
func TestExpiryLoopAppliesWhenDeadlinePasses(t *testing.T) {
	ctrl, mgr, _ := newTestEngine(t)
	id, err := ctrl.AddAccount("alice", model.TierMonthly, 1, 1)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	deadline := time.Now().Add(1100 * time.Millisecond)
	// Round up so the deadline is a whole second still in the future.
	expires := deadline.Unix() + 1
	if err := ctrl.SetExpiry(id, expires); err != nil {
		t.Fatalf("set expiry: %v", err)
	}
	if viewOf(t, ctrl, id).Expired {
		t.Fatal("account expired before its deadline")
	}
	applies := len(mgr.applied)
	ctrl.syncKernel()
	if len(mgr.applied) != applies {
		t.Fatal("syncKernel reconciled although nothing changed")
	}

	time.Sleep(time.Until(time.Unix(expires, 0)) + 50*time.Millisecond)
	ctrl.syncKernel()
	if len(mgr.applied) != applies+1 {
		t.Fatalf("syncKernel should reconcile once after the deadline (applies %d -> %d)", applies, len(mgr.applied))
	}
	if !viewOf(t, ctrl, id).Expired {
		t.Fatal("account should be expired after the deadline")
	}
	ctrl.syncKernel()
	if len(mgr.applied) != applies+1 {
		t.Fatal("a second check after expiry must not reconcile again")
	}
}

// TestSetExpiryRejectsUnlimitedAndUnknown: only tier a/b accounts can expire.
func TestSetExpiryRejectsUnlimitedAndUnknown(t *testing.T) {
	ctrl, _, _ := newTestEngine(t)
	id, err := ctrl.AddAccount("free", model.TierUnlimited, 0, 1)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := ctrl.SetExpiry(id, time.Now().Add(time.Hour).Unix()); err == nil {
		t.Error("tier c account must not accept an expiry")
	}
	if err := ctrl.SetExpiry(id, 0); err != nil {
		t.Errorf("clearing a tier c account's expiry should be allowed: %v", err)
	}
	if err := ctrl.SetExpiry(999, 0); err == nil {
		t.Error("unknown account must be rejected")
	}
	if err := ctrl.SetExpiry(id, -1); err == nil {
		t.Error("negative expiry must be rejected")
	}
}

// TestColdStartRendersPastExpiry: an account whose expiry passed while the
// daemon was down is blocked by the very first Apply on startup.
func TestColdStartRendersPastExpiry(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	id, err := st.CreateAccount(model.Account{Name: "old", Tier: model.TierOneShot, LimitGiB: 1, BillingAnchorDay: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.SetExpiry(id, time.Now().Add(-24*time.Hour).Unix()); err != nil {
		t.Fatalf("set expiry: %v", err)
	}

	mgr := &fakeMgr{exists: false}
	ctrl, err := New(st, mgr, Options{Logf: t.Logf})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	first := mgr.applied[0]
	if a, _ := first.Account(id); !a.ExpiredAt(first.Now) {
		t.Fatal("initial apply should render the expired account as blocked")
	}
	if !viewOf(t, ctrl, id).Expired {
		t.Fatal("view should report the account expired after startup")
	}
}

// TestFailedApplyIsRetried: if the kernel apply fails after SetExpiry has
// written SQLite, the persisted deadline must not be left unenforced. The
// in-memory state keeps following SQLite, the view stays truthful (the kernel
// is not blocking yet), and the next sync retries the apply until it succeeds.
func TestFailedApplyIsRetried(t *testing.T) {
	ctrl, mgr, st := newTestEngine(t)
	id, err := ctrl.AddAccount("alice", model.TierOneShot, 1, 1)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	past := time.Now().Add(-time.Hour).Unix()
	mgr.applyErr = errors.New("netlink: transient failure")
	if err := ctrl.SetExpiry(id, past); err == nil {
		t.Fatal("SetExpiry should report the failed kernel apply")
	}
	snap, _ := st.Load()
	if a, _ := snap.Account(id); a.ExpiresUnix != past {
		t.Fatalf("expiry should be saved despite the failed apply, got %d", a.ExpiresUnix)
	}
	if v := viewOf(t, ctrl, id); v.Account.ExpiresUnix != past || v.Expired {
		t.Fatalf("view = expires %d expired %v; want the saved deadline, not yet enforced", v.Account.ExpiresUnix, v.Expired)
	}

	// Still failing: the retry must not clear the pending state.
	ctrl.syncKernel()
	if viewOf(t, ctrl, id).Expired {
		t.Fatal("account reported expired although no apply succeeded")
	}

	// The kernel recovers: the next sync applies the expiry.
	mgr.applyErr = nil
	applies := len(mgr.applied)
	ctrl.syncKernel()
	if len(mgr.applied) != applies+1 {
		t.Fatalf("sync should retry the failed apply once (applies %d -> %d)", applies, len(mgr.applied))
	}
	if a, _ := mgr.lastApplied().Account(id); a.ExpiresUnix != past {
		t.Fatalf("retried apply carried expiry %d, want %d", a.ExpiresUnix, past)
	}
	if !viewOf(t, ctrl, id).Expired {
		t.Fatal("account should be blocked once the retried apply succeeds")
	}
	ctrl.syncKernel()
	if len(mgr.applied) != applies+1 {
		t.Fatal("no further reconcile once the kernel is back in sync")
	}
}
