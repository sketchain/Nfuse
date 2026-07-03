package rpc

import (
	"path/filepath"
	"testing"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/nft"
	"github.com/sketchain/nfuse/internal/store"
)

// fakeNft is an in-memory nft.Manager: it never touches the kernel, so a real
// engine.Controller can be driven end-to-end in a unit test. TableExists returns
// false (cold start) so reconcile never tries to sample a non-existent table.
type fakeNft struct{}

func (fakeNft) Apply(model.Snapshot) error { return nil }
func (fakeNft) Sample() (nft.Sample, error) {
	return nft.Sample{Counters: map[model.CounterKey]model.Counter{}, AccountUsed: map[int64]uint64{}}, nil
}
func (fakeNft) TableExists() (bool, error) { return false, nil }
func (fakeNft) Teardown() error            { return nil }

// startRealStackServer wires a *real* engine.Controller (backed by a temp SQLite
// store and the in-memory fakeNft) behind a real rpc.Server on a temp socket, and
// returns a connected real rpc.Client. This exercises the cascade semantics
// across the whole client→wire→server→engine→store path, not just field
// forwarding to a fake backend.
func startRealStackServer(t *testing.T) (*Client, *engine.Controller) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nfuse.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctrl, err := engine.New(st, fakeNft{}, engine.Options{Logf: t.Logf})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	sock := filepath.Join(t.TempDir(), "nfuse.sock")
	srv := NewServer(ctrl, "eth0", true, "6.18.5", t.Logf)
	if err := srv.Listen(sock); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })

	cli, err := Dial(sock)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli, ctrl
}

// portsForAccount returns the ports the client's GetState reports for the given
// account id.
func portsForAccount(t *testing.T, cli *Client, id int64) []engine.PortView {
	t.Helper()
	accts, lastErr := cli.View()
	if lastErr != "" {
		t.Fatalf("View lastErr = %q", lastErr)
	}
	for _, a := range accts {
		if a.Account.ID == id {
			return a.Ports
		}
	}
	return nil
}

func accountPresent(t *testing.T, cli *Client, id int64) bool {
	t.Helper()
	accts, lastErr := cli.View()
	if lastErr != "" {
		t.Fatalf("View lastErr = %q", lastErr)
	}
	for _, a := range accts {
		if a.Account.ID == id {
			return true
		}
	}
	return false
}

// TestRealStackCascadeSemantics covers task 2: the cascade flag drives real
// engine behavior over the full RPC stack, not just wire forwarding.
//
//   - An account owning 2 ports refuses DeleteAccount(id, false) and keeps its
//     account + both ports.
//   - DeleteAccount(id, true) removes the account and all of its ports.
//   - After the cascade delete, GetState no longer reports the account or ports.
func TestRealStackCascadeSemantics(t *testing.T) {
	cli, _ := startRealStackServer(t)

	id, err := cli.AddAccount("alice", model.TierMonthly, 10, 15)
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := cli.AddPort(id, 8080, 8080); err != nil {
		t.Fatalf("AddPort 8080: %v", err)
	}
	if err := cli.AddPort(id, 9090, 9090); err != nil {
		t.Fatalf("AddPort 9090: %v", err)
	}
	if got := portsForAccount(t, cli, id); len(got) != 2 {
		t.Fatalf("want 2 ports before delete, got %d (%+v)", len(got), got)
	}

	// cascade=false must be refused while the account still owns ports, and must
	// leave the account and both ports exactly as they were.
	if err := cli.DeleteAccount(id, false); err == nil {
		t.Fatal("DeleteAccount(id, false) succeeded, want refusal while ports exist")
	}
	if !accountPresent(t, cli, id) {
		t.Fatal("account vanished after a refused non-cascade delete")
	}
	if got := portsForAccount(t, cli, id); len(got) != 2 {
		t.Fatalf("want 2 ports still present after refused delete, got %d (%+v)", len(got), got)
	}

	// cascade=true removes the account together with all of its ports.
	if err := cli.DeleteAccount(id, true); err != nil {
		t.Fatalf("DeleteAccount(id, true): %v", err)
	}

	// GetState must no longer report the account (nor, therefore, its ports).
	accts, lastErr := cli.View()
	if lastErr != "" {
		t.Fatalf("View lastErr after cascade delete = %q", lastErr)
	}
	for _, a := range accts {
		if a.Account.ID == id {
			t.Fatalf("account survived cascade delete over RPC: %+v", a)
		}
	}
}

// portID returns the id of the port whose interval starts at start for the given
// account, as reported by the client's GetState.
func portID(t *testing.T, cli *Client, acct int64, start uint16) int64 {
	t.Helper()
	for _, p := range portsForAccount(t, cli, acct) {
		if p.Start == start {
			return p.PortID
		}
	}
	t.Fatalf("no port starting at %d for account %d", start, acct)
	return 0
}

// TestRealStackEditPort covers task 3 over the whole RPC stack (real engine +
// server + client): a port range can be added and edited, the exclude-self
// overlap check permits sliding a range onto its own old extent, and edits that
// collide with another port — same account or cross-account — are rejected.
func TestRealStackEditPort(t *testing.T) {
	cli, _ := startRealStackServer(t)

	a1, err := cli.AddAccount("a1", model.TierMonthly, 10, 15)
	if err != nil {
		t.Fatalf("AddAccount a1: %v", err)
	}
	// Add a range and a second range on the same account.
	if err := cli.AddPort(a1, 60000, 60099); err != nil {
		t.Fatalf("AddPort range: %v", err)
	}
	if err := cli.AddPort(a1, 60200, 60299); err != nil {
		t.Fatalf("AddPort range B: %v", err)
	}
	pa := portID(t, cli, a1, 60000)
	pb := portID(t, cli, a1, 60200)

	// Slide A onto its own old extent: legal (self excluded from the check).
	if err := cli.EditPort(pa, 60001, 60100); err != nil {
		t.Fatalf("self-overlapping edit must succeed: %v", err)
	}
	// GetState reflects the new interval.
	found := false
	for _, p := range portsForAccount(t, cli, a1) {
		if p.PortID == pa {
			found = true
			if p.Start != 60001 || p.End != 60100 {
				t.Fatalf("edited port = %d-%d, want 60001-60100", p.Start, p.End)
			}
		}
	}
	if !found {
		t.Fatalf("edited port %d not reported by GetState", pa)
	}

	// Editing A to overlap B (same account) is rejected.
	if err := cli.EditPort(pa, 60250, 60350); err == nil {
		t.Fatal("editing A to overlap B must be rejected over RPC")
	}

	// Cross-account overlap is rejected too.
	a2, err := cli.AddAccount("a2", model.TierUnlimited, 0, 1)
	if err != nil {
		t.Fatalf("AddAccount a2: %v", err)
	}
	if err := cli.AddPort(a2, 7000, 7000); err != nil {
		t.Fatalf("AddPort a2 single: %v", err)
	}
	p2 := portID(t, cli, a2, 7000)
	if err := cli.EditPort(p2, 60200, 60299); err == nil {
		t.Fatal("editing a2's port to overlap a1's range B must be rejected")
	}
	// B is untouched by the rejected edits.
	if got := portID(t, cli, a1, 60200); got != pb {
		t.Fatalf("port B changed unexpectedly after rejected edits")
	}

	// Editing a non-existent port fails.
	if err := cli.EditPort(999999, 1, 2); err == nil {
		t.Fatal("editing a non-existent port must fail over RPC")
	}
}
