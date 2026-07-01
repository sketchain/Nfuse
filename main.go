// Command nfuse meters per-port bidirectional traffic on a NIC using nftables
// netdev hooks, persists usage to SQLite, enforces per-account quotas with an
// in-kernel circuit breaker, and offers a TUI for management.
//
// Usage:
//
//	nfuse [flags]              run the control plane + TUI
//	nfuse --teardown           remove the kernel ruleset and exit
//	nfuse --headless           run the control plane without the TUI
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
		iface       = flag.String("iface", "ens5", "network interface to meter")
		table       = flag.String("table", "nfuse", "nftables table name")
		dbPath      = flag.String("db", "/var/lib/nfuse/nfuse.db", "SQLite database path")
		sampleIvl   = flag.Duration("sample-interval", 2*time.Second, "kernel counter sampling interval")
		persistIvl  = flag.Duration("persist-interval", 15*time.Second, "SQLite persistence interval")
		refreshIvl  = flag.Duration("ui-refresh", time.Second, "TUI refresh interval")
		headless    = flag.Bool("headless", false, "run without the TUI (control plane only)")
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

	if !*skipKernChk {
		if err := system.CheckNetdevEgress(); err != nil {
			logger.Fatalf("preflight: %v", err)
		}
	}

	mgr := nft.New(*table, *iface)

	if *teardown {
		if err := mgr.Teardown(); err != nil {
			logger.Fatalf("teardown: %v", err)
		}
		logger.Printf("removed nftables table netdev %s", *table)
		return
	}

	if err := ensureDBDir(*dbPath); err != nil {
		logger.Fatalf("db dir: %v", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		logger.Fatalf("open db: %v", err)
	}
	defer st.Close()

	ctrl, err := engine.New(st, mgr, engine.Options{
		SampleInterval:  *sampleIvl,
		PersistInterval: *persistIvl,
		Logf:            logger.Printf,
	})
	if err != nil {
		logger.Fatalf("engine: %v", err)
	}
	ctrl.Start()

	if *headless {
		runHeadless(ctrl, logger)
		return
	}

	ui := tui.New(ctrl, *refreshIvl)
	if err := ui.Run(); err != nil {
		logger.Printf("tui: %v", err)
	}
	ctrl.Stop()
}

func runHeadless(ctrl *engine.Controller, logger *log.Logger) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	logger.Printf("running headless; press Ctrl-C to stop")
	<-sig
	logger.Printf("shutting down")
	ctrl.Stop()
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
