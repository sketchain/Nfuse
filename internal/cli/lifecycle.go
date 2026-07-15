package cli

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/httpapi"
	"github.com/sketchain/nfuse/internal/nft"
	"github.com/sketchain/nfuse/internal/rpc"
	"github.com/sketchain/nfuse/internal/store"
	"github.com/sketchain/nfuse/internal/system"
	"github.com/sketchain/nfuse/internal/tui"
)

// serverOpts carries the daemon configuration parsed from `nfuse server` flags.
type serverOpts struct {
	socket, iface, table, dbPath string
	httpAddr                     string
	httpPort                     int
	sampleIvl, persistIvl        time.Duration
	skipKernChk                  bool
}

// DefaultHTTPAddr is the bind address the HTTP query endpoint uses when it is
// enabled. It defaults to loopback so the endpoint stays local-only unless the
// operator deliberately opens it (e.g. --http-addr 0.0.0.0).
const DefaultHTTPAddr = "127.0.0.1"

// DefaultHTTPPort is the default value of --http-port. It is 0, which means the
// HTTP query endpoint is disabled unless the operator sets a real port; the
// endpoint is opt-in, never started by default.
const DefaultHTTPPort = 0

// runServer is the daemon role (formerly `nfuse --rpc`): it owns nft and SQLite,
// runs the engine, and serves RPCs. It is what systemd keeps running. The
// configuration flags carry the same names and semantics they had on the old
// top-level flag set.
func (a *App) runServer(args []string) int {
	fs := a.newFlagSet("server")
	var (
		socket      = fs.String("socket", DefaultSocket, "unix socket path for client/server RPC")
		iface       = fs.String("iface", "", "network interface to meter (required; e.g. ens5)")
		table       = fs.String("table", "nfuse", "nftables table name")
		dbPath      = fs.String("db", "/var/lib/nfuse/nfuse.db", "SQLite database path")
		sampleIvl   = fs.Duration("sample-interval", 2*time.Second, "kernel counter sampling interval")
		persistIvl  = fs.Duration("persist-interval", 15*time.Second, "SQLite persistence interval")
		skipKernChk = fs.Bool("skip-kernel-check", false, "skip the netdev egress kernel version check")
		httpAddr    = fs.String("http-addr", DefaultHTTPAddr, "HTTP query endpoint bind address (used only when --http-port is set)")
		httpPort    = fs.Int("http-port", DefaultHTTPPort, "HTTP query endpoint port (0 disables the endpoint; the endpoint is off by default)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := log.New(a.Stderr, "nfuse: ", log.LstdFlags)
	if err := validateIface(*iface); err != nil {
		logger.Printf("%v", err)
		return 1
	}
	return runServerDaemon(logger, serverOpts{
		socket: *socket, iface: *iface, table: *table, dbPath: *dbPath,
		httpAddr: *httpAddr, httpPort: *httpPort,
		sampleIvl: *sampleIvl, persistIvl: *persistIvl, skipKernChk: *skipKernChk,
	})
}

// runTUI is the interactive-client role (formerly the bare `nfuse` invocation):
// it connects to the daemon and renders the UI. Without a reachable daemon it
// exits non-zero — the client never runs an embedded engine.
func (a *App) runTUI(args []string) int {
	fs := a.newFlagSet("tui")
	var (
		socket  = fs.String("socket", DefaultSocket, "unix socket path for client/server RPC")
		refresh = fs.Duration("ui-refresh", time.Second, "TUI refresh interval")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := log.New(a.Stderr, "nfuse: ", log.LstdFlags)
	client, err := rpc.Dial(*socket)
	if err != nil {
		logger.Printf("cannot connect to nfuse daemon at %s: %v\nStart the service first: nfuse server --iface <nic>", *socket, err)
		return 1
	}
	defer client.Close()

	ui := tui.New(client, *refresh)
	if err := ui.Run(); err != nil {
		logger.Printf("tui: %v", err)
		return 1
	}
	return 0
}

// runTeardown removes the kernel ruleset (formerly `nfuse --teardown`).
func (a *App) runTeardown(args []string) int {
	fs := a.newFlagSet("teardown")
	var (
		socket = fs.String("socket", DefaultSocket, "unix socket path used to detect a live daemon")
		iface  = fs.String("iface", "", "network interface the ruleset is bound to")
		table  = fs.String("table", "nfuse", "nftables table name")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := log.New(a.Stderr, "nfuse: ", log.LstdFlags)
	// Teardown touches the kernel directly and is a server-side operation. Refuse
	// if a daemon is running: it would immediately start erroring on sampling and
	// rebuild the table from its in-memory snapshot on the next mutation, leaving
	// kernel and daemon state at odds.
	if rpc.DaemonAlive(*socket) {
		logger.Printf("nfuse daemon appears to be running on %s; stop it first "+
			"(systemctl stop nfuse, or kill the `nfuse server` process) before teardown", *socket)
		return 1
	}
	mgr := nft.New(*table, *iface)
	if err := mgr.Teardown(); err != nil {
		logger.Printf("teardown: %v", err)
		return 1
	}
	logger.Printf("removed nftables table netdev %s", *table)
	return 0
}

// validateIface rejects an empty or non-existent interface for the daemon role.
// The daemon must meter a real NIC: an empty name would silently build no useful
// rules, and a name that does not exist would fail later inside `nft -f` with an
// obscure device error. Failing fast here is the expected behavior under systemd
// Restart=on-failure while the NIC is still coming up.
func validateIface(iface string) error {
	if iface == "" {
		return fmt.Errorf("server requires --iface <interface>; pick one from `ip -br link` (e.g. --iface ens5)")
	}
	if _, err := net.InterfaceByName(iface); err != nil {
		return fmt.Errorf("interface %q not found on this host; pick one from `ip -br link`: %v", iface, err)
	}
	return nil
}

// singleInstanceGuard refuses to start a second daemon on the same socket. It is
// called at the very top of runServerDaemon, before any resource (nft table,
// SQLite DB, engine) is touched, so a duplicate daemon is rejected before it can
// mutate the shared kernel/DB state. rpc.Server.Listen performs the same probe
// again as a backstop once it is about to bind.
func singleInstanceGuard(socket string) error {
	if rpc.DaemonAlive(socket) {
		return fmt.Errorf("another nfuse daemon is already listening on %s; stop it first", socket)
	}
	return nil
}

// runServerDaemon is the daemon main loop, extracted so runServer only handles
// flag parsing. It returns the process exit code.
func runServerDaemon(logger *log.Logger, o serverOpts) int {
	// Single-instance guard first, before touching any shared resource: a second
	// daemon must be rejected before engine.New rebuilds the kernel table or the
	// store opens the shared DB.
	if err := singleInstanceGuard(o.socket); err != nil {
		logger.Printf("%v", err)
		return 1
	}

	kernelOK := true
	var kernelRaw string
	if _, _, raw, err := system.KernelVersion(); err == nil {
		kernelRaw = raw
	}
	if !o.skipKernChk {
		if err := system.CheckNetdevEgress(); err != nil {
			logger.Printf("preflight: %v", err)
			return 1
		}
	} else {
		// We skipped enforcement, so we can't assert the kernel is adequate.
		kernelOK = false
	}

	mgr := nft.New(o.table, o.iface)

	if err := ensureDBDir(o.dbPath); err != nil {
		logger.Printf("db dir: %v", err)
		return 1
	}
	st, err := store.Open(o.dbPath)
	if err != nil {
		logger.Printf("open db: %v", err)
		return 1
	}
	defer st.Close()

	ctrl, err := engine.New(st, mgr, engine.Options{
		SampleInterval:  o.sampleIvl,
		PersistInterval: o.persistIvl,
		Logf:            logger.Printf,
	})
	if err != nil {
		logger.Printf("engine: %v", err)
		return 1
	}
	ctrl.Start()
	defer ctrl.Stop()

	srv := rpc.NewServer(ctrl, o.iface, kernelOK, kernelRaw, logger.Printf)
	if err := srv.Listen(o.socket); err != nil {
		logger.Printf("rpc listen: %v", err)
		return 1
	}
	defer srv.Close()

	// HTTP query endpoint: opt-in and server-role only. It starts solely when a
	// port is configured (--http-port > 0); it is off by default. Bind failures
	// are fatal here — same as the RPC socket — so a misconfigured address/port is
	// caught at startup rather than silently swallowed on a background goroutine.
	var httpSrv *httpapi.Server
	var httpAddr string
	if o.httpPort != 0 {
		httpAddr = net.JoinHostPort(o.httpAddr, strconv.Itoa(o.httpPort))
		httpSrv = httpapi.New(ctrl, logger.Printf)
		if err := httpSrv.Listen(httpAddr); err != nil {
			logger.Printf("http listen: %v", err)
			return 1
		}
		defer httpSrv.Close()
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	if httpSrv != nil {
		go func() {
			if err := httpSrv.Serve(); err != nil {
				logger.Printf("http serve: %v", err)
			}
		}()
		logger.Printf("nfuse http query endpoint listening on %s", httpAddr)
	}
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
	return 0
}

// ensureDBDir creates the parent directory of the SQLite database if needed.
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
