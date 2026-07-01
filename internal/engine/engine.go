// Package engine is the user-space control plane: it samples kernel counters and
// quotas, persists them to SQLite off the sampling path, performs tier-a monthly
// resets, and reconciles the nftables ruleset whenever accounts/ports change.
//
// Layering (per the spec's kernel/user-space split):
//
//   - Circuit breaking is entirely in-kernel (the nft quota `drop`). The engine
//     is never on that fast path, so if this process dies the breaker still holds.
//   - Sampling reads kernel state at a human cadence and updates an in-memory
//     view. It never writes SQLite directly.
//   - Persistence runs on its own goroutine/ticker, reading the latest in-memory
//     sample and writing it to WAL-mode SQLite. Thus DB writes never block
//     sampling or the breaker.
//   - Mutations (add/delete account or port, change tier) reconcile the whole
//     ruleset atomically, folding live values back in first so nothing is lost.
package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/nft"
	"github.com/sketchain/nfuse/internal/store"
)

// Controller ties together the store, the nft manager and the sampled state.
type Controller struct {
	store *store.Store
	nft   nft.Manager

	sampleInterval  time.Duration
	persistInterval time.Duration

	mu      sync.Mutex // guards snap and live
	snap    model.Snapshot
	live    nft.Sample // latest kernel reading
	logf    func(string, ...any)
	lastErr string

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options configures a Controller.
type Options struct {
	SampleInterval  time.Duration
	PersistInterval time.Duration
	Logf            func(string, ...any)
}

// New builds a Controller, loads persisted state and applies it to the kernel so
// the ruleset (with seeded counters/quotas) reflects SQLite immediately.
func New(st *store.Store, mgr nft.Manager, opts Options) (*Controller, error) {
	if opts.SampleInterval <= 0 {
		opts.SampleInterval = 2 * time.Second
	}
	if opts.PersistInterval <= 0 {
		opts.PersistInterval = 15 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	c := &Controller{
		store:           st,
		nft:             mgr,
		sampleInterval:  opts.SampleInterval,
		persistInterval: opts.PersistInterval,
		logf:            opts.Logf,
		stopCh:          make(chan struct{}),
	}
	snap, err := st.Load()
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	c.snap = snap
	c.live = nft.Sample{Counters: map[model.CounterKey]model.Counter{}, AccountUsed: map[int64]uint64{}}
	// Seed the kernel from persisted state (backfill after reboot).
	if err := c.nft.Apply(snap); err != nil {
		return nil, fmt.Errorf("apply initial ruleset: %w", err)
	}
	return c, nil
}

// Start launches the background sampling, persistence and reset loops.
func (c *Controller) Start() {
	c.wg.Add(1)
	go c.sampleLoop()
	c.wg.Add(1)
	go c.persistLoop()
}

// Stop halts background loops (and performs a final persist).
func (c *Controller) Stop() {
	close(c.stopCh)
	c.wg.Wait()
	if err := c.persistNow(); err != nil {
		c.logf("final persist: %v", err)
	}
}

func (c *Controller) sampleLoop() {
	defer c.wg.Done()
	t := time.NewTicker(c.sampleInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			s, err := c.nft.Sample()
			if err != nil {
				c.setErr(fmt.Sprintf("sample: %v", err))
				continue
			}
			c.mu.Lock()
			c.live = s
			c.lastErr = ""
			c.mu.Unlock()
		}
	}
}

func (c *Controller) persistLoop() {
	defer c.wg.Done()
	pt := time.NewTicker(c.persistInterval)
	defer pt.Stop()
	// Check resets on a coarse cadence (at most once a minute).
	resetInterval := time.Minute
	if c.persistInterval < resetInterval {
		resetInterval = c.persistInterval
	}
	rt := time.NewTicker(resetInterval)
	defer rt.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-pt.C:
			if err := c.persistNow(); err != nil {
				c.logf("persist: %v", err)
			}
		case <-rt.C:
			c.runDueResets()
		}
	}
}

// persistNow writes the latest sampled values to SQLite. Runs on the persist
// goroutine, never on the sampling path.
func (c *Controller) persistNow() error {
	c.mu.Lock()
	used := make(map[int64]uint64, len(c.live.AccountUsed))
	for id, v := range c.live.AccountUsed {
		used[id] = v
	}
	counters := make(map[model.CounterKey]model.Counter, len(c.live.Counters))
	for k, v := range c.live.Counters {
		counters[k] = v
	}
	// Fold live values into the in-memory desired snapshot too, so a later
	// reconcile seeds from current reality.
	for i := range c.snap.Accounts {
		if u, ok := used[c.snap.Accounts[i].ID]; ok {
			c.snap.Accounts[i].UsedBytes = u
		}
	}
	for k, v := range counters {
		c.snap.Counters[k] = v
	}
	c.mu.Unlock()

	if len(used) == 0 && len(counters) == 0 {
		return nil
	}
	return c.store.PersistUsage(used, counters)
}

func (c *Controller) setErr(msg string) {
	c.mu.Lock()
	c.lastErr = msg
	c.mu.Unlock()
	c.logf("%s", msg)
}

// ── Mutations ────────────────────────────────────────────────────────────────

// reconcile folds live values into SQLite, runs the structural change, reloads
// the desired state and rebuilds the kernel ruleset atomically.
func (c *Controller) reconcile(mutate func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Fold the freshest live values into DB so the rebuild reseeds from them.
	used := make(map[int64]uint64, len(c.live.AccountUsed))
	for id, v := range c.live.AccountUsed {
		used[id] = v
	}
	counters := make(map[model.CounterKey]model.Counter, len(c.live.Counters))
	for k, v := range c.live.Counters {
		counters[k] = v
	}
	if len(used) > 0 || len(counters) > 0 {
		if err := c.store.PersistUsage(used, counters); err != nil {
			return fmt.Errorf("pre-reconcile persist: %w", err)
		}
	}

	if err := mutate(); err != nil {
		return err
	}

	snap, err := c.store.Load()
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return err
	}
	if err := c.nft.Apply(snap); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	c.snap = snap
	return nil
}

// AddAccount creates an account (and its quota object for tier a/b).
func (c *Controller) AddAccount(name string, tier model.Tier, limitGiB float64, anchorDay int) error {
	name, err := model.NormalizeName(name)
	if err != nil {
		return err
	}
	if !tier.Valid() {
		return fmt.Errorf("invalid tier %q", tier)
	}
	if tier.HasQuota() && limitGiB <= 0 {
		return fmt.Errorf("tier %s requires a positive limit", tier)
	}
	if anchorDay < 1 || anchorDay > 28 {
		anchorDay = 1
	}
	return c.reconcile(func() error {
		_, err := c.store.CreateAccount(model.Account{
			Name: name, Tier: tier, LimitGiB: limitGiB, BillingAnchorDay: anchorDay,
		})
		return err
	})
}

// DeleteAccount removes an account; it must own no ports.
func (c *Controller) DeleteAccount(id int64) error {
	c.mu.Lock()
	ports := c.snap.PortsFor(id)
	c.mu.Unlock()
	if len(ports) > 0 {
		return fmt.Errorf("account still owns %d port(s); remove them first", len(ports))
	}
	return c.reconcile(func() error { return c.store.DeleteAccount(id) })
}

// SetTier switches an account's tier, adjusting its quota object accordingly.
func (c *Controller) SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error {
	if !tier.Valid() {
		return fmt.Errorf("invalid tier %q", tier)
	}
	if tier.HasQuota() && limitGiB <= 0 {
		return fmt.Errorf("tier %s requires a positive limit", tier)
	}
	if anchorDay < 1 || anchorDay > 28 {
		anchorDay = 1
	}
	return c.reconcile(func() error { return c.store.SetTier(id, tier, limitGiB, anchorDay) })
}

// AddPort attaches a port to an account, creating its in/out counters and rules.
func (c *Controller) AddPort(accountID int64, port uint16) error {
	if port == 0 {
		return fmt.Errorf("port must be non-zero")
	}
	c.mu.Lock()
	_, ok := c.snap.Account(accountID)
	for _, p := range c.snap.Ports {
		if p.Port == port {
			c.mu.Unlock()
			return fmt.Errorf("port %d already managed by account %d", port, p.AccountID)
		}
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("account %d not found", accountID)
	}
	return c.reconcile(func() error {
		_, err := c.store.CreatePort(model.Port{AccountID: accountID, Port: port})
		return err
	})
}

// DeletePort removes a port and reclaims its counters/rules.
func (c *Controller) DeletePort(portID int64) error {
	return c.reconcile(func() error { return c.store.DeletePort(portID) })
}

// ResetAccount zeroes a quota account's usage now (manual tier-a reset, also
// usable to re-arm a spent tier-b account).
func (c *Controller) ResetAccount(id int64) error {
	c.mu.Lock()
	acct, ok := c.snap.Account(id)
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("account %d not found", id)
	}
	if !acct.Tier.HasQuota() {
		return fmt.Errorf("account %q has no quota to reset", acct.Name)
	}
	if err := c.nft.ResetQuota(id); err != nil {
		return err
	}
	now := time.Now()
	if err := c.store.MarkReset(id, now); err != nil {
		return err
	}
	c.mu.Lock()
	for i := range c.snap.Accounts {
		if c.snap.Accounts[i].ID == id {
			c.snap.Accounts[i].UsedBytes = 0
			c.snap.Accounts[i].LastResetUnix = now.Unix()
		}
	}
	if c.live.AccountUsed != nil {
		c.live.AccountUsed[id] = 0
	}
	c.mu.Unlock()
	return nil
}

// runDueResets performs automatic monthly resets for tier-a accounts whose
// billing cycle boundary has passed.
func (c *Controller) runDueResets() {
	now := time.Now()
	c.mu.Lock()
	var due []int64
	for _, a := range c.snap.Accounts {
		if !a.Tier.Resets() {
			continue
		}
		last := time.Unix(a.LastResetUnix, 0)
		if !now.Before(nextReset(last, a.BillingAnchorDay)) {
			due = append(due, a.ID)
		}
	}
	c.mu.Unlock()
	for _, id := range due {
		if err := c.ResetAccount(id); err != nil {
			c.logf("monthly reset of account %d: %v", id, err)
		} else {
			c.logf("monthly reset applied to account %d", id)
		}
	}
}

// nextReset returns the next billing boundary strictly after last for the given
// day-of-month anchor.
func nextReset(last time.Time, anchorDay int) time.Time {
	if anchorDay < 1 || anchorDay > 28 {
		anchorDay = 1
	}
	y, m, _ := last.Date()
	c := time.Date(y, m, anchorDay, 0, 0, 0, 0, last.Location())
	if !c.After(last) {
		c = c.AddDate(0, 1, 0)
	}
	return c
}

// ── Views for the TUI ────────────────────────────────────────────────────────

// PortView is a per-port line combining desired state with live counters.
type PortView struct {
	PortID   int64
	Port     uint16
	InBytes  uint64
	OutBytes uint64
}

// AccountView is an account line combining desired state with live usage.
type AccountView struct {
	Account   model.Account
	UsedBytes uint64 // live quota used (tier a/b) or summed counters (tier c)
	Ports     []PortView
}

// Snapshot returns a consistent view for rendering. It merges the desired-state
// tree with the latest kernel sample.
func (c *Controller) View() ([]AccountView, string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	views := make([]AccountView, 0, len(c.snap.Accounts))
	for _, a := range c.snap.Accounts {
		av := AccountView{Account: a}
		var sum uint64
		for _, p := range c.snap.PortsFor(a.ID) {
			in := c.live.Counters[model.CounterKey{PortID: p.ID, Dir: model.DirIn}].Bytes
			out := c.live.Counters[model.CounterKey{PortID: p.ID, Dir: model.DirOut}].Bytes
			av.Ports = append(av.Ports, PortView{PortID: p.ID, Port: p.Port, InBytes: in, OutBytes: out})
			sum += in + out
		}
		if a.Tier.HasQuota() {
			if u, ok := c.live.AccountUsed[a.ID]; ok {
				av.UsedBytes = u
			} else {
				av.UsedBytes = a.UsedBytes
			}
		} else {
			av.UsedBytes = sum // unlimited: informational total
		}
		views = append(views, av)
	}
	return views, c.lastErr
}

// Teardown removes the kernel ruleset (used on --teardown; DB is untouched).
func (c *Controller) Teardown() error { return c.nft.Teardown() }
