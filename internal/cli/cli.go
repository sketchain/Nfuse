// Package cli is the command-line front end for nfuse. It turns the binary into
// a subcommand-driven tool whose first-class citizens are the operational
// commands (list/add/rm/…) that drive the daemon over its Unix-socket RPC; the
// interactive TUI is one subcommand among them (`nfuse tui`).
//
// Command families:
//
//	Lifecycle — server (the engine daemon, formerly `--rpc`), tui, teardown,
//	            version. These own the process role; see lifecycle.go.
//	Operations — list, add, rm, set-tier, reset, set-usage, port {add,edit,rm,
//	            move}, persist. Each is a thin UDS RPC client that maps onto one
//	            method of rpc.Client; see commands.go.
//
// The socket is the ONLY communication surface: operational commands connect to
// the daemon and never touch nft/SQLite themselves.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/sketchain/nfuse/internal/rpc"
)

// DefaultSocket is the Unix socket path shared by the daemon and every client
// command unless overridden with --socket.
const DefaultSocket = "/run/nfuse.sock"

// BuildInfo carries the linker-injected version metadata for `nfuse version`.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// App holds the wiring for a single CLI invocation. Streams and the client
// dialer are injected so the operational commands can be exercised in tests
// against a fake daemon without a live socket.
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	// Dial connects to the daemon and returns a client for the operational
	// commands. In production this is rpc.Dial adapted to the opClient interface.
	Dial  func(socket string) (opClient, error)
	Build BuildInfo
}

// NewApp returns an App wired to the real process streams and the real RPC
// dialer.
func NewApp(build BuildInfo) *App {
	return &App{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Dial: func(socket string) (opClient, error) {
			return rpc.Dial(socket)
		},
		Build: build,
	}
}

// Main is the process entry point: it dispatches args[0] to a subcommand and
// returns the process exit code (0 success, non-zero failure). A bare `nfuse`
// with no subcommand prints usage and exits non-zero — it no longer launches
// the TUI (use `nfuse tui`).
func Main(build BuildInfo, args []string) int {
	return NewApp(build).Run(args)
}

// Run dispatches a parsed argument list. It is the testable core of Main.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		a.usage(a.Stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	// Lifecycle roles.
	case "server":
		return a.runServer(rest)
	case "tui":
		return a.runTUI(rest)
	case "teardown":
		return a.runTeardown(rest)
	case "version":
		return a.runVersion(rest)

	// Operational commands (UDS RPC clients).
	case "list":
		return a.cmdList(rest)
	case "add":
		return a.cmdAdd(rest)
	case "rm":
		return a.cmdRm(rest)
	case "set-tier":
		return a.cmdSetTier(rest)
	case "reset":
		return a.cmdReset(rest)
	case "set-usage":
		return a.cmdSetUsage(rest)
	case "port":
		return a.cmdPort(rest)
	case "persist":
		return a.cmdPersist(rest)
	case "token":
		return a.cmdToken(rest)

	case "-h", "--help", "help":
		a.usage(a.Stdout)
		return 0
	default:
		fmt.Fprintf(a.Stderr, "nfuse: unknown command %q\n\n", cmd)
		a.usage(a.Stderr)
		return 2
	}
}

func (a *App) runVersion(args []string) int {
	fs := a.newFlagSet("version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintf(a.Stdout, "nfuse %s (commit %s, built %s, %s)\n",
		a.Build.Version, a.Build.Commit, a.Build.Date, runtime.Version())
	return 0
}

// newFlagSet builds a ContinueOnError flag set whose usage/errors go to the
// App's stderr, so a parse failure returns a non-zero exit code instead of
// calling os.Exit (which flag.ExitOnError would do).
func (a *App) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	return fs
}

// parseArgs parses fs while allowing flags and positional arguments to be
// interleaved (e.g. `add <name> --tier c`). The stdlib flag package stops at
// the first non-flag token, so we drive it in a loop: parse up to the next
// positional, stash that positional, then resume on the remainder. The returned
// slice is the positional arguments in order.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return positional, nil
}

func (a *App) usage(w io.Writer) {
	fmt.Fprint(w, `nfuse — per-port traffic metering with in-kernel quotas

Usage:
  nfuse <command> [flags]

Lifecycle:
  server      run the engine daemon (owns nft + SQLite); requires --iface
  tui         launch the interactive terminal UI against a running daemon
  teardown    remove the nftables ruleset and exit
  version     print version information

Accounts:
  list        list accounts, ports and usage (--json for machine output)
  add         add an account:       nfuse add <name> --tier a|b|c [--limit GiB] [--anchor 1-28]
  rm          delete an account:     nfuse rm <account> [--cascade]
  set-tier    change tier/limit:     nfuse set-tier <account> --tier a|b|c [--limit GiB] [--anchor 1-28]
  reset       zero an account's usage:   nfuse reset <account>
  set-usage   set an account's usage:    nfuse set-usage <account> <bytes>
  persist     force a SQLite snapshot now

Ports:
  port add    nfuse port add  <account> <start[-end]>
  port edit   nfuse port edit <port-id> <start[-end]>
  port rm     nfuse port rm   <port-id>
  port move   nfuse port move <port-id> <account>

Tokens (HTTP query, curl <host:port>/<token>):
  token show   <account>   print an account's query token
  token new    <account>   regenerate an account's query token
  token master [--new]     print (or regenerate) the master token

<account> accepts an account name or numeric id; <port-id> is the numeric id
shown by `+"`nfuse list`"+`. Operational commands share --socket (default `+DefaultSocket+`).
Run `+"`nfuse <command> -h`"+` for command-specific flags.
`)
}
