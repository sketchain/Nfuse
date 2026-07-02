package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/nft"
)

// gatedMgr is a fakeMgr whose first Sample() call blocks on a gate, so a test
// can deterministically wedge a mutation *between* the sample being issued and
// its result being written back — reproducing the stale-overwrite race (P0-1).
type gatedMgr struct {
	fakeMgr
	mu      sync.Mutex
	calls   int
	started chan struct{} // closed when the first Sample() begins
	release chan struct{} // closed to let the first Sample() return
	stale   nft.Sample    // returned by the first (gated) Sample()
	fresh   nft.Sample    // returned by every later Sample()
}

func (m *gatedMgr) Sample() (nft.Sample, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	if n == 1 {
		close(m.started)
		<-m.release
		return m.stale, nil
	}
	return m.fresh, nil
}

// TestStaleSampleDoesNotOverwriteReset drives the P0-1 race: a sample is taken,
// a ResetAccount lands while it is in flight, and the (now stale) sample returns
// afterwards. The generation guard must discard it so the reset is not silently
// reverted by a resurrected pre-reset usage value.
func TestStaleSampleDoesNotOverwriteReset(t *testing.T) {
	st, id := seedDB(t)
	defer st.Close()

	mgr := &gatedMgr{
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale: nft.Sample{
			Counters:    map[model.CounterKey]model.Counter{},
			AccountUsed: map[int64]uint64{id: 999},
		},
		fresh: nft.Sample{
			Counters:    map[model.CounterKey]model.Counter{},
			AccountUsed: map[int64]uint64{id: 0},
		},
	}
	// Cold start so New() neither samples nor adopts; the sampleLoop is the only
	// caller of Sample() in this test.
	mgr.exists = false

	c, err := New(st, mgr, Options{SampleInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start()
	defer c.Stop()

	// Wait until a sample is in flight (taken at the current generation).
	<-mgr.started
	// A usage-lowering mutation completes while that sample is blocked.
	if err := c.ResetAccount(id); err != nil {
		t.Fatalf("ResetAccount: %v", err)
	}
	// Now let the stale sample (used=999) return; the loop must discard it.
	close(mgr.release)

	// Give the loop time to process the stale sample and take a fresh one.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if usedFor(c, id) == 0 {
			// Confirm it stays 0 (stale value never wins on a later tick either).
			time.Sleep(30 * time.Millisecond)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := usedFor(c, id); got != 0 {
		t.Fatalf("stale sample overwrote reset: used=%d, want 0", got)
	}
}

func usedFor(c *Controller, id int64) uint64 {
	views, _ := c.View()
	for _, v := range views {
		if v.Account.ID == id {
			return v.UsedBytes
		}
	}
	return 0
}

// TestReconcileFoldsFreshSample drives P0-2: a mutation must fold a *fresh*
// kernel sample (not the last periodic one) into SQLite and the rebuild, so the
// traffic metered since the last sample is not lost.
func TestReconcileFoldsFreshSample(t *testing.T) {
	st, id := seedDB(t) // tier-b account, persisted used=100
	defer st.Close()

	// Cold start: New() applies once without sampling, leaving c.live empty.
	mgr := &fakeMgr{exists: false}
	c, err := New(st, mgr, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The table is now live and the kernel holds newer usage than anything the
	// engine has sampled (c.live is still empty).
	mgr.exists = true
	mgr.sample = nft.Sample{
		Counters:    map[model.CounterKey]model.Counter{},
		AccountUsed: map[int64]uint64{id: 777},
	}

	// Any mutation triggers reconcile, which must fresh-sample first.
	if err := c.AddPort(id, 8080); err != nil {
		t.Fatalf("AddPort: %v", err)
	}
	if mgr.sampled == 0 {
		t.Fatalf("reconcile must take a fresh sample before folding")
	}

	acct, _ := mgr.lastApplied().Account(id)
	if acct.UsedBytes != 777 {
		t.Errorf("rebuild seeded used=%d, want 777 (fresh kernel sample)", acct.UsedBytes)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dbAcct, _ := snap.Account(id)
	if dbAcct.UsedBytes != 777 {
		t.Errorf("SQLite used=%d after reconcile, want 777 (folded fresh sample)", dbAcct.UsedBytes)
	}
}

// TestReconcileAbortsOnSampleFailure covers the P0-2 failure policy: if the
// table exists but sampling fails, reconcile must abort rather than rebuild from
// stale accounting.
func TestReconcileAbortsOnSampleFailure(t *testing.T) {
	st, id := seedDB(t)
	defer st.Close()

	mgr := &errSampleMgr{}
	mgr.exists = false
	c, err := New(st, mgr, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mgr.exists = true // now sampling will be attempted, and it fails

	if err := c.AddPort(id, 8080); err == nil {
		t.Fatalf("reconcile should abort when a live table cannot be sampled")
	}
}

// errSampleMgr is a fakeMgr whose Sample() always fails.
type errSampleMgr struct{ fakeMgr }

func (m *errSampleMgr) Sample() (nft.Sample, error) {
	return nft.Sample{}, errSample
}

type sampleErr struct{}

func (sampleErr) Error() string { return "sample boom" }

var errSample = sampleErr{}
