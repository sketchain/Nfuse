package engine

import (
	"path/filepath"
	"testing"

	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/nft"
	"github.com/sketchain/nfuse/internal/store"
)

// fakeMgr is an nft.Manager that records applies and reports a scripted table
// existence / live sample, so the cold-vs-hot restart branch can be tested
// without a real kernel.
type fakeMgr struct {
	exists  bool
	sample  nft.Sample
	applied []model.Snapshot
	sampled int
}

func (m *fakeMgr) Apply(s model.Snapshot) error {
	// Deep-copy the account slice so later mutations don't rewrite history.
	cp := s
	cp.Accounts = append([]model.Account(nil), s.Accounts...)
	m.applied = append(m.applied, cp)
	return nil
}
func (m *fakeMgr) Sample() (nft.Sample, error) { m.sampled++; return m.sample, nil }
func (m *fakeMgr) TableExists() (bool, error)  { return m.exists, nil }
func (m *fakeMgr) Teardown() error             { return nil }

func (m *fakeMgr) lastApplied() model.Snapshot { return m.applied[len(m.applied)-1] }

// seedDB creates one tier-b account with the given persisted used bytes.
func seedDB(t *testing.T) (*store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id, err := st.CreateAccount(model.Account{Name: "acct", Tier: model.TierOneShot, LimitGiB: 10})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.PersistUsage(map[int64]uint64{id: 100}, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	return st, id
}

// Cold start (table absent): SQLite is authoritative; the seed comes from the DB
// value and the kernel is not sampled.
func TestNewColdStartSeedsFromSQLite(t *testing.T) {
	st, id := seedDB(t)
	defer st.Close()
	mgr := &fakeMgr{exists: false, sample: nft.Sample{
		Counters: map[model.CounterKey]model.Counter{}, AccountUsed: map[int64]uint64{id: 999},
	}}
	if _, err := New(st, mgr, Options{}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if mgr.sampled != 0 {
		t.Errorf("cold start must not sample the kernel, sampled=%d", mgr.sampled)
	}
	acct, _ := mgr.lastApplied().Account(id)
	if acct.UsedBytes != 100 {
		t.Errorf("cold start seeded used=%d, want 100 (SQLite)", acct.UsedBytes)
	}
}

// Hot restart (table present): the kernel is authoritative; live usage overrides
// the staler SQLite value both in the rebuild seed and back in the DB.
func TestNewHotRestartAdoptsKernel(t *testing.T) {
	st, id := seedDB(t)
	defer st.Close()
	mgr := &fakeMgr{exists: true, sample: nft.Sample{
		Counters: map[model.CounterKey]model.Counter{}, AccountUsed: map[int64]uint64{id: 999},
	}}
	if _, err := New(st, mgr, Options{}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if mgr.sampled == 0 {
		t.Errorf("hot restart must sample the live kernel state")
	}
	acct, _ := mgr.lastApplied().Account(id)
	if acct.UsedBytes != 999 {
		t.Errorf("hot restart seeded used=%d, want 999 (kernel)", acct.UsedBytes)
	}
	// The adopted value must also be folded back into SQLite.
	snap, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dbAcct, _ := snap.Account(id)
	if dbAcct.UsedBytes != 999 {
		t.Errorf("SQLite used=%d after hot restart, want 999", dbAcct.UsedBytes)
	}
}
