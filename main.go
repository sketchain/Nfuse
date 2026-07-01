// Command nfuse meters per-port bidirectional traffic on a NIC using nftables
// netdev hooks, persists usage to SQLite, and enforces per-account quotas with
// an in-kernel circuit breaker. It runs in one of two roles from the same
// binary:
//
//	nfuse --rpc                run the server daemon: engine + RPC socket, no TUI.
//	                           This is the ONLY process that touches nft/SQLite;
//	                           systemd keeps it running.
//	nfuse                      run the client: connect to the daemon's socket and
//	                           render the TUI. Touches neither the kernel nor the
//	                           DB — every action is an RPC. If the daemon is not
//	                           running it errors out (no embedded-engine fallback).
//	nfuse --teardown           remove the kernel ruleset and exit.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/nft"
	"github.com/sketchain/nfuse/internal/rpc"
	"github.com/sketchain/nfuse/internal/store"
	"github.com/sketchain/nfuse/internal/system"
	"github.com/sketchain/nfuse/internal/tui"
)

// Build metadata, overridable at link time via
// -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		rpcMode     = flag.Bool("rpc", false, "run as the server daemon (engine + RPC socket, no TUI)")
		socket      = flag.String("socket", "/run/nfuse.sock", "unix socket path for client/server RPC")
		iface       = flag.String("iface", "ens5", "network interface to meter")
		table       = flag.String("table", "nfuse", "nftables table name")
		dbPath      = flag.String("db", "/var/lib/nfuse/nfuse.db", "SQLite database path")
		sampleIvl   = flag.Duration("sample-interval", 2*time.Second, "kernel counter sampling interval")
		persistIvl  = flag.Duration("persist-interval", 15*time.Second, "SQLite persistence interval")
		refreshIvl  = flag.Duration("ui-refresh", time.Second, "TUI refresh interval")
		teardown    = flag.Bool("teardown", false, "remove the nftables ruleset and exit")
		skipKernChk = flag.Bool("skip-kernel-check", false, "skip the netdev egress kernel version check")
		showVer     = flag.Bool("version", false, "print version information and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("nfuse %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
		return
	}

	logger := log.New(os.Stderr, "nfuse: ", log.LstdFlags)

	if *teardown {
		// Teardown touches the kernel directly and is a server-side operation.
		mgr := nft.New(*table, *iface)
		if err := mgr.Teardown(); err != nil {
			logger.Fatalf("teardown: %v", err)
		}
		logger.Printf("removed nftables table netdev %s", *table)
		return
	}

	if *rpcMode {
		runServer(logger, serverOpts{
			socket: *socket, iface: *iface, table: *table, dbPath: *dbPath,
			sampleIvl: *sampleIvl, persistIvl: *persistIvl, skipKernChk: *skipKernChk,
		})
		return
	}

	runClient(logger, *socket, *refreshIvl)
}

type serverOpts struct {
	socket, iface, table, dbPath string
	sampleIvl, persistIvl        time.Duration
	skipKernChk                  bool
}

// runServer is the daemon role: it owns nft and SQLite, runs the engine, and
// serves RPCs. It is what systemd keeps running.
func runServer(logger *log.Logger, o serverOpts) {
	kernelOK := true
	var kernelRaw string
	if _, _, raw, err := system.KernelVersion(); err == nil {
		kernelRaw = raw
	}
	if !o.skipKernChk {
		if err := system.CheckNetdevEgress(); err != nil {
			logger.Fatalf("preflight: %v", err)
		}
	} else {
		// We skipped enforcement, so we can't assert the kernel is adequate.
		kernelOK = false
	}

	mgr := nft.New(o.table, o.iface)

	if err := ensureDBDir(o.dbPath); err != nil {
		logger.Fatalf("db dir: %v", err)
	}
	st, err := store.Open(o.dbPath)
	if err != nil {
		logger.Fatalf("open db: %v", err)
	}
	defer st.Close()

	ctrl, err := engine.New(st, mgr, engine.Options{
		SampleInterval:  o.sampleIvl,
		PersistInterval: o.persistIvl,
		Logf:            logger.Printf,
	})
	if err != nil {
		logger.Fatalf("engine: %v", err)
	}
	ctrl.Start()
	defer ctrl.Stop()

	srv := rpc.NewServer(ctrl, o.iface, kernelOK, kernelRaw, logger.Printf)
	if err := srv.Listen(o.socket); err != nil {
		logger.Fatalf("rpc listen: %v", err)
	}
	defer srv.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	logger.Printf("nfuse daemon listening on %s (iface %s); press Ctrl-C to stop", o.socket, o.iface)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		logger.Printf("shutting down")
	case err := <-errCh:
		if err != nil {
			logger.Printf("rpc serve: %v", err)
		}
	}
}

// runClient is the TUI role: it connects to the daemon and renders the UI.
// Without a reachable daemon it exits with an error — the client never runs an
// embedded engine.
func runClient(logger *log.Logger, socket string, refresh time.Duration) {
	client, err := rpc.Dial(socket)
	if err != nil {
		logger.Fatalf("cannot connect to nfuse daemon at %s: %v\nStart the service first: nfuse --rpc", socket, err)
	}
	defer client.Close()

	ui := tui.New(client, refresh)
	if err := ui.Run(); err != nil {
		logger.Printf("tui: %v", err)
	}
}

func ensureDBDir(path string) error {
	dir := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			dir = path[:i]
			break
		}
	}
	if dir == "" || dir == path {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}
