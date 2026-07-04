package cli

import (
	"net"
	"path/filepath"
	"testing"
)

// TestSingleInstanceGuardRejectsLiveDaemon: a second daemon must be rejected by
// the top-of-runServerDaemon guard — which runs before nft.New/store.Open/
// engine.New touch any shared resource — as soon as a live daemon answers on the
// socket. We stand up a listener to play the first daemon and assert the guard
// errors. (Carried over from the pre-restructure main_test.go, which lost this
// coverage when the guard moved into internal/cli.)
func TestSingleInstanceGuardRejectsLiveDaemon(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nfuse.sock")

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// Accept and drop connections so DaemonAlive's probe dial succeeds.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if err := singleInstanceGuard(socket); err == nil {
		t.Fatal("singleInstanceGuard accepted startup while a daemon is listening")
	}
}

// TestSingleInstanceGuardAllowsFreeSocket: with nothing listening, the guard must
// permit startup (a stale/absent socket is handled later by Listen).
func TestSingleInstanceGuardAllowsFreeSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nfuse.sock")
	if err := singleInstanceGuard(socket); err != nil {
		t.Fatalf("singleInstanceGuard rejected a free socket: %v", err)
	}
}
