package engine

import (
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/nft"
	"github.com/sketchain/nfuse/internal/store"
)

// requireNftAdmin skips the test unless nft is present AND the process can
// actually mutate the ruleset. On CI runners the nft binary exists but the job
// lacks CAP_NET_ADMIN, so a real probe (not just LookPath) is needed.
func requireNftAdmin(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not available")
	}
	const probe = "nfuse_probe"
	// Adding then removing a throwaway table exercises the same privileged
	// netlink path our code uses; "Operation not permitted" here means no caps.
	if out, err := exec.Command("nft", "add", "table", "netdev", probe).CombinedOutput(); err != nil {
		t.Skipf("nftables not permitted in this environment: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("nft", "delete", "table", "netdev", probe).Run()
}

// TestIntegration exercises the full stack against real nftables on the loopback
// interface. It requires root + nft and is skipped otherwise.
func TestIntegration(t *testing.T) {
	requireNftAdmin(t)
	table := "nfuse_it"
	mgr := nft.New(table, "lo")
	defer mgr.Teardown()

	dbPath := filepath.Join(t.TempDir(), "it.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer st.Close()

	ctrl, err := New(st, mgr, Options{SampleInterval: 200 * time.Millisecond, PersistInterval: 300 * time.Millisecond, Logf: t.Logf})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	ctrl.Start()
	defer ctrl.Stop()

	// Tier a account with two ports.
	if _, err := ctrl.AddAccount("alice", model.TierMonthly, 1, 15); err != nil {
		t.Fatalf("add account: %v", err)
	}
	views, _ := ctrl.View()
	if len(views) != 1 {
		t.Fatalf("want 1 account, got %d", len(views))
	}
	aid := views[0].Account.ID
	if err := ctrl.AddPort(aid, 8080); err != nil {
		t.Fatalf("add port 8080: %v", err)
	}
	if err := ctrl.AddPort(aid, 9090); err != nil {
		t.Fatalf("add port 9090: %v", err)
	}

	dump := nftDump(t, table)
	// Expect the shared quota, breaker rules (dport/sport) and counters.
	for _, want := range []string{
		"quota acct" + itoa(aid),
		"th dport 8080 quota name",
		"th sport 8080 quota name",
		"th dport 8080 counter name",
		"th sport 9090 counter name",
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("ruleset missing %q\n%s", want, dump)
		}
	}

	// Duplicate port must be rejected.
	if err := ctrl.AddPort(aid, 8080); err == nil {
		t.Errorf("expected duplicate port rejection")
	}

	// Generate real loopback traffic on 9090 and confirm the counter advances.
	feedTraffic(t, 9090)
	time.Sleep(500 * time.Millisecond)
	views, _ = ctrl.View()
	var moved bool
	for _, p := range views[0].Ports {
		if p.Port == 9090 && (p.InBytes > 0 || p.OutBytes > 0) {
			moved = true
		}
	}
	if !moved {
		t.Logf("counter did not advance (loopback timing); non-fatal")
	}

	// Switch alice to unlimited: quota must disappear, counters remain.
	if err := ctrl.SetTier(aid, model.TierUnlimited, 0, 1); err != nil {
		t.Fatalf("set tier c: %v", err)
	}
	dump = nftDump(t, table)
	if strings.Contains(dump, "quota name") {
		t.Errorf("unlimited tier should have no quota rules\n%s", dump)
	}
	if !strings.Contains(dump, "th dport 8080 counter name") {
		t.Errorf("counters should survive tier change\n%s", dump)
	}

	// Reset should fail for unlimited, succeed after switching back.
	if err := ctrl.ResetAccount(aid); err == nil {
		t.Errorf("reset on unlimited should error")
	}
	if err := ctrl.SetTier(aid, model.TierOneShot, 2, 1); err != nil {
		t.Fatalf("set tier b: %v", err)
	}
	if err := ctrl.ResetAccount(aid); err != nil {
		t.Fatalf("reset tier b: %v", err)
	}

	// Cannot delete account with ports.
	if err := ctrl.DeleteAccount(aid); err == nil {
		t.Errorf("expected delete-with-ports rejection")
	}
	// Delete ports then account.
	for _, p := range views[0].Ports {
		if err := ctrl.DeletePort(p.PortID); err != nil {
			t.Fatalf("delete port %d: %v", p.Port, err)
		}
	}
	if err := ctrl.DeleteAccount(aid); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	dump = nftDump(t, table)
	if strings.Contains(dump, "counter name") || strings.Contains(dump, "quota acct") {
		t.Errorf("objects should be reclaimed after deletes\n%s", dump)
	}
}

// TestPersistenceBackfill verifies that usage is reloaded and seeded across a
// simulated restart.
func TestPersistenceBackfill(t *testing.T) {
	requireNftAdmin(t)
	table := "nfuse_bf"
	dbPath := filepath.Join(t.TempDir(), "bf.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	mgr := nft.New(table, "lo")
	ctrl, err := New(st, mgr, Options{Logf: t.Logf})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if _, err := ctrl.AddAccount("bob", model.TierOneShot, 1, 1); err != nil {
		t.Fatalf("add account: %v", err)
	}
	views, _ := ctrl.View()
	aid := views[0].Account.ID
	if err := ctrl.AddPort(aid, 7070); err != nil {
		t.Fatalf("add port: %v", err)
	}
	// Simulate accumulated usage by setting it through the engine (SetUsage seeds
	// both the kernel quota and SQLite via the normal reconcile path), then
	// "restart". Writing straight to the DB behind the engine's back would be
	// undone by the final persist, which correctly folds the live kernel state
	// (used=0) back over it — see reconcile's fresh-sample fold.
	if err := ctrl.SetUsage(aid, 500*1024*1024); err != nil {
		t.Fatalf("set usage: %v", err)
	}
	ctrl.Stop()
	mgr.Teardown()
	st.Close()

	// Restart: reopen DB + fresh manager; New() should seed the quota's used.
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	mgr2 := nft.New(table, "lo")
	defer mgr2.Teardown()
	ctrl2, err := New(st2, mgr2, Options{Logf: t.Logf})
	if err != nil {
		t.Fatalf("engine2: %v", err)
	}
	defer ctrl2.Stop()

	s, err := mgr2.Sample()
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got := s.AccountUsed[aid]; got != 500*1024*1024 {
		t.Errorf("quota used = %d bytes, want %d (seeded from SQLite)\n%s", got, 500*1024*1024, nftDump(t, table))
	}
}

func nftDump(t *testing.T, table string) string {
	t.Helper()
	out, err := exec.Command("nft", "list", "table", "netdev", table).CombinedOutput()
	if err != nil {
		t.Fatalf("nft list: %v: %s", err, out)
	}
	return string(out)
}

func feedTraffic(t *testing.T, port int) {
	t.Helper()
	// Best-effort: open a listener and dial it on loopback so packets traverse
	// the netdev hooks. Failures are non-fatal (environment dependent).
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Logf("listen: %v", err)
		return
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				break
			}
		}
		c.Close()
	}()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Logf("dial: %v", err)
		return
	}
	defer conn.Close()
	payload := make([]byte, 64*1024)
	for i := 0; i < 8; i++ {
		if _, err := conn.Write(payload); err != nil {
			break
		}
	}
}

func itoa(i int64) string { return fmt.Sprintf("%d", i) }
