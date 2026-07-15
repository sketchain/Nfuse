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
	"crypto/subtle"
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

	mu          sync.Mutex // guards snap, live, lastPersist, gen, masterToken
	snap        model.Snapshot
	live        nft.Sample // latest kernel reading
	logf        func(string, ...any)
	lastErr     string
	startedAt   time.Time // process start (for health uptime)
	lastPersist time.Time // last successful SQLite snapshot
	masterToken string    // HTTP query token that returns every account

	// gen is a monotonic generation counter, bumped whenever a mutation changes
	// the kernel's expected quota/counter values (reconcile, ResetAccount,
	// SetUsage). The sampling loop records it before issuing a (slow, lock-free)
	// Sample() and re-checks it before writing the result back: if it advanced in
	// the meantime, a mutation lowered usage while the sample was in flight, so
	// the sample is stale and discarded to avoid silently reviving old usage.
	gen uint64

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
		startedAt:       time.Now(),
		stopCh:          make(chan struct{}),
	}
	snap, err := st.Load()
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	// The master token is created/backfilled by the store's migration, so it is
	// present by the time the engine loads it here.
	master, err := st.MasterToken()
	if err != nil {
		return nil, fmt.Errorf("load master token: %w", err)
	}
	c.masterToken = master
	c.live = nft.Sample{Counters: map[model.CounterKey]model.Counter{}, AccountUsed: map[int64]uint64{}}

	// Cold start vs hot restart: probe whether our kernel table already exists.
	//
	//   - Table absent  => cold start (the machine rebooted, kernel state is
	//     empty). SQLite is authoritative: build the table seeding counters and
	//     quotas from the persisted snapshot.
	//   - Table present => hot restart (this process was relaunched, e.g. by
	//     systemd Restart=on-failure, while the box stayed up). The kernel still
	//     holds live usage newer than SQLite, so the kernel is authoritative: we
	//     sample the live values, fold them into the snapshot (and SQLite), and
	//     rebuild seeding from those — never letting stale SQLite overwrite them.
	exists, err := c.nft.TableExists()
	if err != nil {
		return nil, fmt.Errorf("probe kernel table: %w", err)
	}
	if exists {
		if err := c.adoptLiveState(&snap); err != nil {
			return nil, err
		}
	}
	c.snap = snap
	if err := c.nft.Apply(snap); err != nil {
		return nil, fmt.Errorf("apply initial ruleset: %w", err)
	}
	return c, nil
}

// adoptLiveState samples the live kernel counters/quotas (hot restart) and folds
// them over the persisted snapshot so the rebuild reseeds from current reality,
// also persisting them back to SQLite so the DB reflects the kernel truth.
func (c *Controller) adoptLiveState(snap *model.Snapshot) error {
	s, err := c.nft.Sample()
	if err != nil {
		return fmt.Errorf("sample live state on hot restart: %w", err)
	}
	for i := range snap.Accounts {
		if u, ok := s.AccountUsed[snap.Accounts[i].ID]; ok {
			snap.Accounts[i].UsedBytes = u
		}
	}
	for k, v := range s.Counters {
		snap.Counters[k] = v
	}
	c.live = s
	// Fold the adopted live values into SQLite so a later reconcile (and the DB
	// on disk) start from the kernel's newer numbers, not the stale snapshot.
	if len(s.AccountUsed) > 0 || len(s.Counters) > 0 {
		if err := c.store.PersistUsage(s.AccountUsed, s.Counters); err != nil {
			return fmt.Errorf("persist adopted live state: %w", err)
		}
		c.lastPersist = time.Now()
	}
	return nil
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
			// Record the generation before the (slow, lock-free) sample so we can
			// tell if a usage-lowering mutation completed while we were reading.
			c.mu.Lock()
			startGen := c.gen
			c.mu.Unlock()
			s, err := c.nft.Sample()
			if err != nil {
				c.setErr(fmt.Sprintf("sample: %v", err))
				continue
			}
			c.mu.Lock()
			// Drop the sample if a mutation changed expected kernel state while it
			// was in flight; the next tick will re-sample the reconciled values.
			if c.gen == startGen {
				c.live = s
			}
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
//
// The DB write below happens outside c.mu and without a gen guard, and that is
// safe: kernel counters and quota "used" bytes only ever increase between
// mutations, so a stale persist can at worst write a slightly-old value that the
// next sample immediately heals upward. The one operation that *lowers* usage —
// a reset — is the sole exception, and it is now funneled through reconcile
// (under c.mu, with gen++), so persistNow can never race a reset into reviving
// pre-reset usage.
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
		// Nothing to write (e.g. no accounts yet), but this still counts as a
		// successful persist tick: record it so health reflects a live persister
		// instead of "never persisted", and skip the empty DB transaction.
		c.mu.Lock()
		c.lastPersist = time.Now()
		c.mu.Unlock()
		return nil
	}
	if err := c.store.PersistUsage(used, counters); err != nil {
		return err
	}
	c.mu.Lock()
	c.lastPersist = time.Now()
	c.mu.Unlock()
	return nil
}

// ForcePersist triggers an immediate SQLite snapshot (the ForcePersist RPC).
func (c *Controller) ForcePersist() error { return c.persistNow() }

// Stats reports process start and last successful persist times (for health).
func (c *Controller) Stats() (startedAt, lastPersist time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startedAt, c.lastPersist
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

	// Take a *fresh* sample before folding. The periodic sampleLoop value can be
	// up to a sample-interval old, so the traffic metered between the last sample
	// and this rebuild would be lost from both counters and quota on every
	// mutation. Sampling here (like adoptLiveState does on hot restart) captures
	// it. If the table is present but sampling fails we refuse the mutation
	// rather than rebuild from stale accounting and silently drop that traffic;
	// if the table is absent (e.g. before the first account exists) there is
	// nothing to sample, so we continue.
	exists, err := c.nft.TableExists()
	if err != nil {
		return fmt.Errorf("probe table before reconcile: %w", err)
	}
	if exists {
		s, err := c.nft.Sample()
		if err != nil {
			return fmt.Errorf("fresh sample before reconcile: %w", err)
		}
		c.live = s
	}

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
	// This rebuild reseeded the kernel's quota/counter values, so any sample the
	// sampleLoop took before now is stale: bump the generation to discard it.
	c.gen++
	return nil
}

// AddAccount creates an account (and its quota object for tier a/b), returning
// the new account id.
func (c *Controller) AddAccount(name string, tier model.Tier, limitGiB float64, anchorDay int) (int64, error) {
	name, err := model.NormalizeName(name)
	if err != nil {
		return 0, err
	}
	if !tier.Valid() {
		return 0, fmt.Errorf("invalid tier %q", tier)
	}
	if tier.HasQuota() && limitGiB <= 0 {
		return 0, fmt.Errorf("tier %s requires a positive limit", tier)
	}
	if anchorDay < 1 || anchorDay > 28 {
		return 0, fmt.Errorf("billing anchor day must be between 1 and 28, got %d", anchorDay)
	}
	var newID int64
	err = c.reconcile(func() error {
		// Generate the query token inside the reconcile closure, where c.mu is held
		// and c.snap is the serialized truth, so the uniqueness check races nothing.
		token, err := c.uniqueTokenLocked()
		if err != nil {
			return err
		}
		id, err := c.store.CreateAccount(model.Account{
			Name: name, Tier: tier, LimitGiB: limitGiB, BillingAnchorDay: anchorDay, Token: token,
		})
		newID = id
		return err
	})
	return newID, err
}

// uniqueTokenLocked returns a fresh query token distinct from every account
// token and the master token. The caller must hold c.mu (reconcile, or the
// regenerate paths), so the view of existing tokens is consistent.
func (c *Controller) uniqueTokenLocked() (string, error) {
	used := make(map[string]bool, len(c.snap.Accounts)+1)
	if c.masterToken != "" {
		used[c.masterToken] = true
	}
	for _, a := range c.snap.Accounts {
		if a.Token != "" {
			used[a.Token] = true
		}
	}
	for {
		t, err := model.GenerateToken()
		if err != nil {
			return "", err
		}
		if !used[t] {
			return t, nil
		}
	}
}

// RegenerateToken issues a fresh query token for an account and returns it. A
// token change does not affect the kernel ruleset, so this takes a lighter path
// than reconcile: it updates SQLite and the in-memory account under c.mu without
// resampling or rebuilding the nft table. The old token stops working at once.
func (c *Controller) RegenerateToken(id int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := -1
	for i := range c.snap.Accounts {
		if c.snap.Accounts[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return "", fmt.Errorf("account %d not found", id)
	}
	token, err := c.uniqueTokenLocked()
	if err != nil {
		return "", err
	}
	if err := c.store.SetToken(id, token); err != nil {
		return "", err
	}
	c.snap.Accounts[idx].Token = token
	return token, nil
}

// MasterToken returns the master query token (the one that returns every
// account's usage in a single query).
func (c *Controller) MasterToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.masterToken
}

// RegenerateMasterToken issues a fresh master query token and returns it. Like
// RegenerateToken it bypasses reconcile — no kernel state is involved.
func (c *Controller) RegenerateMasterToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	token, err := c.uniqueTokenLocked()
	if err != nil {
		return "", err
	}
	if err := c.store.SetMasterToken(token); err != nil {
		return "", err
	}
	c.masterToken = token
	return token, nil
}

// QueryByToken resolves an HTTP query token to the account views it grants
// access to. The master token returns every account (all == true); an account
// token returns just that account. ok is false for an empty or unknown token.
// The comparison is constant-time to avoid leaking token bytes through timing.
func (c *Controller) QueryByToken(token string) (views []AccountView, all bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token == "" {
		return nil, false, false
	}
	if c.masterToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(c.masterToken)) == 1 {
		return c.viewLocked(), true, true
	}
	for _, a := range c.snap.Accounts {
		if a.Token == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) == 1 {
			for _, v := range c.viewLocked() {
				if v.Account.ID == a.ID {
					return []AccountView{v}, false, true
				}
			}
		}
	}
	return nil, false, false
}

// DeleteAccount removes an account. When cascade is false it refuses an account
// that still owns ports (the caller must remove them first); when cascade is
// true it deletes the account together with all of its ports (and their
// counters) atomically inside a single reconcile — the store's DELETE relies on
// the schema's ON DELETE CASCADE, so the whole subtree is removed in one step,
// never as a sequence of separate RPCs.
//
// The port guard runs *inside* the reconcile closure, where c.snap is the
// serialized post-fold truth. Checking it out here (before reconcile) would
// leave a window in which a concurrent AddPort could slip a port in after the
// check but before the DELETE, and cascade=false would then silently drop that
// fresh port via ON DELETE CASCADE. Reconcile holds c.mu for the whole
// sample→fold→mutate→apply sequence, and a mutate error short-circuits before
// Apply/gen++, so a rejected delete has no side effects.
func (c *Controller) DeleteAccount(id int64, cascade bool) error {
	return c.reconcile(func() error {
		if ports := c.snap.PortsFor(id); len(ports) > 0 && !cascade {
			return fmt.Errorf("account still owns %d port(s); remove them first", len(ports))
		}
		return c.store.DeleteAccount(id)
	})
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
		return fmt.Errorf("billing anchor day must be between 1 and 28, got %d", anchorDay)
	}
	// Note: switching tiers deliberately does NOT clear used_bytes. Moving an
	// account a/b → c only *pauses* metering; moving back revives the historical
	// usage and re-seeds it into the kernel quota. Use ResetAccount to zero it.
	return c.reconcile(func() error { return c.store.SetTier(id, tier, limitGiB, anchorDay) })
}

// AddPort attaches a port (or a contiguous range) to an account, creating its
// in/out counters and rules. An end of 0 is shorthand for a single port
// (end = start), preserving the pre-range call convention.
//
// The account-exists and interval-overlap checks run *inside* the reconcile
// closure, against c.snap (the serialized post-fold truth), following the
// DeleteAccount precedent: reconcile holds c.mu for the whole
// sample→fold→mutate→apply sequence, so two concurrent AddPorts can never both
// pass an overlap check and then both insert overlapping ranges — the loser sees
// the winner's row in c.snap and is rejected before any side effect.
func (c *Controller) AddPort(accountID int64, start, end uint16) error {
	if end == 0 {
		end = start
	}
	np := model.Port{AccountID: accountID, Start: start, End: end}
	if !np.ValidRange() {
		return fmt.Errorf("invalid port range %s (need 1 ≤ start ≤ end)", np)
	}
	return c.reconcile(func() error {
		if _, ok := c.snap.Account(accountID); !ok {
			return fmt.Errorf("account %d not found", accountID)
		}
		for _, p := range c.snap.Ports {
			if p.Overlaps(np) {
				return fmt.Errorf("port range %s overlaps existing %s (account %d)", np, p, p.AccountID)
			}
		}
		_, err := c.store.CreatePort(np)
		return err
	})
}

// EditPort rewrites an existing port's interval (renumber a single port, shift a
// range's bounds, or convert between single and range). The port keeps its id, so
// its counters — named by port id — survive the reconcile rebuild with their
// accumulated values intact (metering continuity across an edit).
//
// The overlap check excludes the port being edited: sliding a range so its new
// extent overlaps its own old extent (e.g. 60000-60099 → 60001-60100) is a legal
// move, not a conflict. Like AddPort, the check runs inside the reconcile closure
// against c.snap so it is atomic against concurrent mutations.
func (c *Controller) EditPort(portID int64, start, end uint16) error {
	if end == 0 {
		end = start
	}
	np := model.Port{ID: portID, Start: start, End: end}
	if !np.ValidRange() {
		return fmt.Errorf("invalid port range %s (need 1 ≤ start ≤ end)", np)
	}
	return c.reconcile(func() error {
		var found bool
		for _, p := range c.snap.Ports {
			if p.ID == portID {
				found = true
				continue
			}
			if p.Overlaps(np) {
				return fmt.Errorf("port range %s overlaps existing %s (account %d)", np, p, p.AccountID)
			}
		}
		if !found {
			return fmt.Errorf("port %d not found", portID)
		}
		return c.store.EditPort(portID, start, end)
	})
}

// DeletePort removes a port and reclaims its counters/rules.
func (c *Controller) DeletePort(portID int64) error {
	return c.reconcile(func() error { return c.store.DeletePort(portID) })
}

// MovePort reassigns a port to another account. The port keeps its counters;
// the reconcile re-points its rules at the destination account's quota (or none,
// if the destination is unlimited).
func (c *Controller) MovePort(portID, newAccountID int64) error {
	c.mu.Lock()
	_, ok := c.snap.Account(newAccountID)
	var found bool
	for _, p := range c.snap.Ports {
		if p.ID == portID {
			found = true
			break
		}
	}
	c.mu.Unlock()
	if !found {
		return fmt.Errorf("port %d not found", portID)
	}
	if !ok {
		return fmt.Errorf("account %d not found", newAccountID)
	}
	return c.reconcile(func() error { return c.store.MovePort(portID, newAccountID) })
}

// SetUsage overwrites an account's recorded quota usage to an arbitrary target
// (higher or lower). It updates SQLite and re-seeds the kernel quota to the same
// value through the standard reconcile path. Because reconcile folds the live
// (pre-change) value into SQLite *before* running the mutation, the mutation's
// write is the one that survives into the rebuild — so the seeded quota ends up
// at exactly the requested target.
func (c *Controller) SetUsage(id int64, usedBytes uint64) error {
	c.mu.Lock()
	acct, ok := c.snap.Account(id)
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("account %d not found", id)
	}
	if !acct.Tier.HasQuota() {
		return fmt.Errorf("account %q is unlimited and has no usage to set", acct.Name)
	}
	return c.reconcile(func() error {
		if err := c.store.SetUsage(id, usedBytes); err != nil {
			return err
		}
		// Keep the in-memory live view consistent until the next sample re-reads
		// the freshly re-seeded kernel quota, so an immediately following
		// reconcile does not fold the stale value back in.
		if c.live.AccountUsed != nil {
			c.live.AccountUsed[id] = usedBytes
		}
		return nil
	})
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
	// Route the reset through reconcile like SetUsage. reconcile folds the live
	// (pre-reset) usage into SQLite *before* running the mutation, so MarkReset's
	// zeroing write is the one that survives into the reload; the rebuild's Apply
	// then re-seeds the kernel quota's used bytes to 0 from that reloaded
	// snapshot — no separate nft.ResetQuota call is needed. Doing this under c.mu
	// with the reconcile's gen++ closes the race where a concurrent persistNow /
	// reconcile could revive the pre-reset usage (SQLite records the old value
	// while the kernel quota was already zeroed out-of-band).
	return c.reconcile(func() error {
		if err := c.store.MarkReset(id, time.Now()); err != nil {
			return err
		}
		// Keep the in-memory live view consistent until the next sample re-reads
		// the freshly re-seeded kernel quota, so an immediately following
		// reconcile does not fold the stale value back in.
		if c.live.AccountUsed != nil {
			c.live.AccountUsed[id] = 0
		}
		return nil
	})
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

// PortView is a per-port line combining desired state with live counters. Start
// and End describe the port's closed interval (Start == End for a single port).
type PortView struct {
	PortID   int64
	Start    uint16
	End      uint16
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
	return c.viewLocked(), c.lastErr
}

// viewLocked builds the merged desired-state + live-sample view. The caller must
// hold c.mu. It backs both View (for the TUI/RPC) and QueryByToken (for HTTP).
func (c *Controller) viewLocked() []AccountView {
	views := make([]AccountView, 0, len(c.snap.Accounts))
	for _, a := range c.snap.Accounts {
		av := AccountView{Account: a}
		var sum uint64
		for _, p := range c.snap.PortsFor(a.ID) {
			in := c.live.Counters[model.CounterKey{PortID: p.ID, Dir: model.DirIn}].Bytes
			out := c.live.Counters[model.CounterKey{PortID: p.ID, Dir: model.DirOut}].Bytes
			av.Ports = append(av.Ports, PortView{PortID: p.ID, Start: p.Start, End: p.End, InBytes: in, OutBytes: out})
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
	return views
}

// Teardown removes the kernel ruleset (used on --teardown; DB is untouched).
func (c *Controller) Teardown() error { return c.nft.Teardown() }
