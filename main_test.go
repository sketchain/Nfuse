package main

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// TestSingleInstanceGuardRejectsLiveDaemon covers task 2: a second daemon must be
// rejected by the top-of-runServer guard — which runs before nft.New/store.Open/
// engine.New touch any shared resource — as soon as a live daemon answers on the
// socket. We stand up a listener to play the first daemon and assert the guard
// errors; runServer calls this exact function before creating the engine.
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

// TestValidateIface covers task 1: the daemon role requires a concrete, existing
// interface. An empty name and a non-existent name must both be rejected with a
// helpful message; loopback (always present) must pass.
func TestValidateIface(t *testing.T) {
	if err := validateIface(""); err == nil {
		t.Error("empty --iface must be rejected for --rpc")
	} else if !strings.Contains(err.Error(), "--iface") {
		t.Errorf("empty-iface error should mention --iface: %v", err)
	}

	if err := validateIface("definitely-not-a-real-nic0"); err == nil {
		t.Error("non-existent --iface must be rejected")
	}

	if _, err := net.InterfaceByName("lo"); err == nil {
		if err := validateIface("lo"); err != nil {
			t.Errorf("validateIface(lo) = %v, want nil", err)
		}
	}
}
