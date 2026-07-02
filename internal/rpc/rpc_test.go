package rpc

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// fakeBackend is an in-memory Backend recording calls, used to exercise the
// server/client wire protocol without a live kernel.
type fakeBackend struct {
	mu       sync.Mutex
	accounts []engine.AccountView
	lastErr  string
	nextID   int64

	setUsage    map[int64]uint64
	moved       map[int64]int64
	deleted     []deleteCall
	persisted   int
	started     time.Time
	lastPersist time.Time
}

// deleteCall records one DeleteAccount invocation so tests can assert the
// cascade flag was forwarded over the wire.
type deleteCall struct {
	id      int64
	cascade bool
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		setUsage: map[int64]uint64{},
		moved:    map[int64]int64{},
		started:  time.Now().Add(-time.Minute),
	}
}

func (f *fakeBackend) View() ([]engine.AccountView, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accounts, f.lastErr
}

func (f *fakeBackend) AddAccount(name string, tier model.Tier, limitGiB float64, anchorDay int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.accounts = append(f.accounts, engine.AccountView{
		Account: model.Account{ID: f.nextID, Name: name, Tier: tier, LimitGiB: limitGiB, BillingAnchorDay: anchorDay},
	})
	return f.nextID, nil
}

func (f *fakeBackend) DeleteAccount(id int64, cascade bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, deleteCall{id: id, cascade: cascade})
	return nil
}
func (f *fakeBackend) SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error {
	return nil
}
func (f *fakeBackend) AddPort(accountID int64, port uint16) error { return nil }
func (f *fakeBackend) DeletePort(portID int64) error              { return nil }

func (f *fakeBackend) MovePort(portID, newAccountID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moved[portID] = newAccountID
	return nil
}

func (f *fakeBackend) ResetAccount(id int64) error { return nil }

func (f *fakeBackend) SetUsage(id int64, usedBytes uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setUsage[id] = usedBytes
	return nil
}

func (f *fakeBackend) ForcePersist() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persisted++
	f.lastPersist = time.Now()
	return nil
}

func (f *fakeBackend) Stats() (time.Time, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.lastPersist
}

// startTestServer spins up a Server on a temp socket and returns a connected
// client plus a cleanup func.
func startTestServer(t *testing.T, be Backend) (*Client, func()) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "nfuse.sock")
	srv := NewServer(be, "eth0", true, "6.18.5", t.Logf)
	if err := srv.Listen(sock); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()
	cli, err := Dial(sock)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	return cli, func() { cli.Close(); srv.Close() }
}

func TestClientServerRoundTrip(t *testing.T) {
	be := newFakeBackend()
	cli, cleanup := startTestServer(t, be)
	defer cleanup()

	// AddAccount returns the new id.
	id, err := cli.AddAccount("alice", model.TierMonthly, 10, 15)
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if id != 1 {
		t.Fatalf("AddAccount id = %d, want 1", id)
	}

	// GetState reflects the mutation.
	accts, lastErr := cli.View()
	if lastErr != "" {
		t.Fatalf("View lastErr = %q", lastErr)
	}
	if len(accts) != 1 || accts[0].Account.Name != "alice" || accts[0].Account.ID != 1 {
		t.Fatalf("View = %+v", accts)
	}

	// SetUsage forwards the target bytes verbatim.
	if err := cli.SetUsage(1, 12345); err != nil {
		t.Fatalf("SetUsage: %v", err)
	}
	if got := be.setUsage[1]; got != 12345 {
		t.Fatalf("backend SetUsage = %d, want 12345", got)
	}

	// DeleteAccount forwards the cascade flag both ways over the wire.
	if err := cli.DeleteAccount(1, true); err != nil {
		t.Fatalf("DeleteAccount cascade: %v", err)
	}
	if err := cli.DeleteAccount(2, false); err != nil {
		t.Fatalf("DeleteAccount no-cascade: %v", err)
	}
	be.mu.Lock()
	gotDeletes := append([]deleteCall(nil), be.deleted...)
	be.mu.Unlock()
	want := []deleteCall{{id: 1, cascade: true}, {id: 2, cascade: false}}
	if len(gotDeletes) != len(want) || gotDeletes[0] != want[0] || gotDeletes[1] != want[1] {
		t.Fatalf("backend DeleteAccount calls = %+v, want %+v", gotDeletes, want)
	}

	// MovePort forwards both ids.
	if err := cli.MovePort(7, 1); err != nil {
		t.Fatalf("MovePort: %v", err)
	}
	if got := be.moved[7]; got != 1 {
		t.Fatalf("backend MovePort dest = %d, want 1", got)
	}

	// ForcePersist reaches the backend.
	if err := cli.ForcePersist(); err != nil {
		t.Fatalf("ForcePersist: %v", err)
	}
	if be.persisted != 1 {
		t.Fatalf("persisted = %d, want 1", be.persisted)
	}

	// Health carries the static host facts and a positive uptime.
	h, err := cli.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.Alive || h.Iface != "eth0" || !h.KernelOK || h.KernelVersion != "6.18.5" {
		t.Fatalf("Health = %+v", h)
	}
	if h.UptimeSeconds <= 0 {
		t.Fatalf("Health uptime = %v, want > 0", h.UptimeSeconds)
	}
}

// TestListenRefusesLiveDaemon covers P1-5: a second daemon must not steal a
// socket a live daemon already owns, and the first daemon must keep working.
func TestListenRefusesLiveDaemon(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nfuse.sock")
	be := newFakeBackend()

	s1 := NewServer(be, "eth0", true, "6.18.5", nil)
	if err := s1.Listen(sock); err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	go s1.Serve()
	defer s1.Close()

	// A second instance on the same live socket must be refused.
	s2 := NewServer(be, "eth0", true, "6.18.5", nil)
	if err := s2.Listen(sock); err == nil {
		s2.Close()
		t.Fatal("second Listen succeeded, want refusal while a daemon is live")
	}

	// The first daemon's socket must remain intact and usable.
	cli, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial first daemon after refused takeover: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Health(); err != nil {
		t.Fatalf("first daemon broken after refused takeover: %v", err)
	}
}

// TestClientReconnects covers P1-4: an open client transparently recovers when
// the daemon is restarted under it (systemd Restart=on-failure).
func TestClientReconnects(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nfuse.sock")
	be := newFakeBackend()

	s1 := NewServer(be, "eth0", true, "6.18.5", nil)
	if err := s1.Listen(sock); err != nil {
		t.Fatalf("listen s1: %v", err)
	}
	go s1.Serve()

	cli, err := Dial(sock)
	if err != nil {
		s1.Close()
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Health(); err != nil {
		t.Fatalf("initial Health: %v", err)
	}

	// Restart the daemon on the same socket.
	s1.Close()
	s2 := NewServer(be, "eth0", true, "6.18.5", nil)
	if err := s2.Listen(sock); err != nil {
		t.Fatalf("relisten s2: %v", err)
	}
	go s2.Serve()
	defer s2.Close()

	// The next call must reconnect and succeed without the caller re-dialing.
	if _, err := cli.Health(); err != nil {
		t.Fatalf("client did not reconnect after daemon restart: %v", err)
	}
}

func TestServerReportsError(t *testing.T) {
	be := &errBackend{}
	cli, cleanup := startTestServer(t, be)
	defer cleanup()

	err := cli.DeleteAccount(99, false)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("DeleteAccount err = %v, want boom", err)
	}
}

// errBackend fails every mutation to check error propagation over the wire.
type errBackend struct{ fakeBackend }

func (e *errBackend) DeleteAccount(id int64, cascade bool) error { return errBoom }

var errBoom = boomErr{}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }
