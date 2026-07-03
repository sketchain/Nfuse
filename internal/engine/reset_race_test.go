package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/nft"
)

// faithfulMgr models the kernel honestly enough for the reset-race test: Apply
// re-seeds each quota account's "used" bytes from the snapshot exactly as the
// real nft ruleset does (so a reset that reloads a zeroed snapshot lands the
// kernel quota back at 0), and Sample returns those seeded values. Traffic is
// simulated by the test writing `used` directly. All state is mutex-guarded so
// `go test -race` exercises the engine's locking, not the fake's.
type faithfulMgr struct {
	mu     sync.Mutex
	used   map[int64]uint64
	exists bool
}

func newFaithfulMgr() *faithfulMgr { return &faithfulMgr{used: map[int64]uint64{}} }

func (m *faithfulMgr) Apply(s model.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range s.Accounts {
		if a.Tier.HasQuota() {
			m.used[a.ID] = a.UsedBytes
		}
	}
	m.exists = true
	return nil
}

func (m *faithfulMgr) Sample() (nft.Sample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	au := make(map[int64]uint64, len(m.used))
	for k, v := range m.used {
		au[k] = v
	}
	return nft.Sample{Counters: map[model.CounterKey]model.Counter{}, AccountUsed: au}, nil
}

func (m *faithfulMgr) TableExists() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exists, nil
}

func (m *faithfulMgr) Teardown() error { return nil }

func (m *faithfulMgr) setUsed(id int64, v uint64) {
	m.mu.Lock()
	m.used[id] = v
	m.mu.Unlock()
}

func (m *faithfulMgr) usedOf(id int64) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used[id]
}

// TestConcurrentResetAndPersistKeepsDBZero is the regression guard for the
// reset data race. ResetAccount used to zero the kernel quota and MarkReset
// SQLite *outside* c.mu, so a concurrent persistNow (which folds the live,
// pre-reset usage into SQLite off-lock and without a gen guard) could write the
// old usage back over the reset — leaving SQLite holding a value the reset had
// cleared, which the next reconcile/restart would reseed into the kernel and
// silently undo the reset.
//
// Now ResetAccount runs through reconcile, owning c.mu for the whole
// sample→fold→MarkReset→reload→Apply sequence, so it serializes against
// persistNow. This test hammers ResetAccount against ForcePersist for thousands
// of rounds and asserts that after each round SQLite (and the kernel) show the
// account's usage as 0.
func TestConcurrentResetAndPersistKeepsDBZero(t *testing.T) {
	st, id := seedDB(t)
	defer st.Close()

	mgr := newFaithfulMgr()
	// Long intervals + no Start(): we drive reset/persist by hand so every round
	// is deterministic; background ticks would only add noise.
	c, err := New(st, mgr, Options{SampleInterval: time.Hour, PersistInterval: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const rounds = 3000
	const traffic = uint64(1) << 30 // 1 GiB of usage to resurrect if the race reopens

	for i := 0; i < rounds; i++ {
		// Spend the quota: DB, kernel and the live view all now show usage, so a
		// leaking persist has a concrete non-zero value to write back.
		if err := c.SetUsage(id, traffic); err != nil {
			t.Fatalf("round %d SetUsage: %v", i, err)
		}
		if mgr.usedOf(id) != traffic {
			t.Fatalf("round %d: kernel not seeded with traffic (setup bug)", i)
		}
		// Belt and braces: make sure the kernel genuinely reports the spent value
		// even if a prior Apply raced (it doesn't here, but keep the intent clear).
		mgr.setUsed(id, traffic)

		// Reset and persist race each other.
		var wg sync.WaitGroup
		wg.Add(2)
		var resetErr, persistErr error
		go func() { defer wg.Done(); resetErr = c.ResetAccount(id) }()
		go func() { defer wg.Done(); persistErr = c.ForcePersist() }()
		wg.Wait()
		if resetErr != nil {
			t.Fatalf("round %d ResetAccount: %v", i, resetErr)
		}
		if persistErr != nil {
			t.Fatalf("round %d ForcePersist: %v", i, persistErr)
		}

		// A persist that copied the pre-reset value under the lock may still have
		// been mid-write when the reset finished (persistNow's DB write is off-lock
		// by design). The reset zeroed the live view, so the next persist heals
		// SQLite — the self-healing property the persistNow comment relies on. Do
		// that heal synchronously so the round's end state is well defined.
		if err := c.ForcePersist(); err != nil {
			t.Fatalf("round %d heal persist: %v", i, err)
		}

		snap, err := st.Load()
		if err != nil {
			t.Fatalf("round %d load: %v", i, err)
		}
		acct, ok := snap.Account(id)
		if !ok {
			t.Fatalf("round %d: account %d vanished", i, id)
		}
		if acct.UsedBytes != 0 {
			t.Fatalf("round %d: SQLite used_bytes=%d after reset, want 0 (race revived usage)", i, acct.UsedBytes)
		}
		// The kernel invariant is what pins the fix to the reconcile path: only
		// ResetAccount's reload+Apply zeroes the kernel quota, so a reset that
		// skipped reconcile would leave the kernel non-zero here.
		if k := mgr.usedOf(id); k != 0 {
			t.Fatalf("round %d: kernel quota used=%d after reset, want 0 (reset did not reseed)", i, k)
		}
	}
}
