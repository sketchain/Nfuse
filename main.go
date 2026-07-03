// Command nfuse meters per-port bidirectional traffic on a NIC using nftables
// netdev hooks, persists usage to SQLite, and enforces per-account quotas with
// an in-kernel circuit breaker. The same binary is a subcommand-driven CLI whose
// first-class citizens are the account/port operations; the interactive TUI is
// one subcommand among them:
//
//	nfuse server --iface X     run the engine daemon: engine + RPC socket, no TUI.
//	                           This is the ONLY process that touches nft/SQLite;
//	                           systemd keeps it running.
//	nfuse tui                  connect to the daemon's socket and render the TUI.
//	nfuse list | add | rm | …  drive one daemon RPC each (see `nfuse help`).
//	nfuse teardown             remove the kernel ruleset and exit.
//
// All command wiring lives in internal/cli; main only injects build metadata.
package main

import (
	"os"

	"github.com/sketchain/nfuse/internal/cli"
)

// Build metadata, overridable at link time via
// -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(cli.Main(cli.BuildInfo{Version: version, Commit: commit, Date: date}, os.Args[1:]))
}
