package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// opClient is the subset of rpc.Client that the operational commands drive. It
// is an interface so the commands can be tested against a fake daemon; the real
// *rpc.Client satisfies it. Every method maps to an existing RPC — the CLI is a
// pure consumer and adds no new protocol.
type opClient interface {
	View() ([]engine.AccountView, string)
	AddAccount(name string, tier model.Tier, limitGiB float64, anchorDay int) (int64, error)
	DeleteAccount(id int64, cascade bool) error
	SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error
	AddPort(accountID int64, start, end uint16) error
	EditPort(portID int64, start, end uint16) error
	DeletePort(portID int64) error
	MovePort(portID, newAccountID int64) error
	ResetAccount(id int64) error
	SetUsage(id int64, usedBytes uint64) error
	ForcePersist() error
	RegenerateToken(id int64) (string, error)
	MasterToken() (string, error)
	RegenerateMasterToken() (string, error)
	Close() error
}

// socketFlag registers the shared --socket flag on fs and returns the target.
func socketFlag(fs *flag.FlagSet) *string {
	return fs.String("socket", DefaultSocket, "unix socket path for the daemon RPC")
}

// arg returns the i-th positional argument, or "" if there is none.
func arg(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

// exactArgs verifies pos carries exactly n positional arguments. On mismatch it
// prints usage to stderr — distinguishing "too few" (show the command's own
// usage string) from "too many" (extra tokens the flag parser would otherwise
// drop silently) — and returns false so the caller exits 2. Every operational
// subcommand has a fixed arity, so this is the single gate for both cases.
func (a *App) exactArgs(pos []string, n int, usage string) bool {
	switch {
	case len(pos) < n:
		fmt.Fprintf(a.Stderr, "nfuse: %s\n", usage)
	case len(pos) > n:
		fmt.Fprintf(a.Stderr, "nfuse: too many arguments (want %d, got %d); %s\n", n, len(pos), usage)
	default:
		return true
	}
	return false
}

// withClient dials the daemon and runs fn, translating a dial failure into a
// non-zero exit code with a message on stderr. It centralizes the connect /
// close / error-reporting boilerplate shared by every operational command.
func (a *App) withClient(socket string, fn func(c opClient) error) int {
	c, err := a.Dial(socket)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: cannot connect to daemon at %s: %v\n", socket, err)
		return 1
	}
	defer c.Close()
	if err := fn(c); err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 1
	}
	return 0
}

// ── list ─────────────────────────────────────────────────────────────────────

func (a *App) cmdList(args []string) int {
	fs := a.newFlagSet("list")
	socket := socketFlag(fs)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON on stdout")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 0, "list takes no positional arguments: nfuse list [--json]") {
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		accounts, statusErr := c.View()
		// View surfaces a fatal round-trip failure as (nil, err-string); a mere
		// last-sampling error comes back alongside real account data. Treat the
		// former as an error, the latter as a stderr warning that still yields data.
		if accounts == nil && statusErr != "" {
			return fmt.Errorf("%s", statusErr)
		}
		if statusErr != "" {
			fmt.Fprintf(a.Stderr, "nfuse: warning: %s\n", statusErr)
		}
		if *asJSON {
			return a.writeJSON(accounts)
		}
		a.writeHuman(accounts)
		return nil
	})
}

// ── add ──────────────────────────────────────────────────────────────────────

func (a *App) cmdAdd(args []string) int {
	fs := a.newFlagSet("add")
	socket := socketFlag(fs)
	tierStr := fs.String("tier", "", "account tier: a (monthly), b (one-shot), c (unlimited)")
	limit := fs.Float64("limit", 0, "quota limit in GiB (required for tier a/b, ignored for c)")
	anchor := fs.Int("anchor", 1, "monthly billing anchor day, 1-28 (tier a)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 1, "add requires an account name: nfuse add <name> --tier a|b|c") {
		return 2
	}
	name := arg(pos, 0)
	tier, err := parseTier(*tierStr)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	if err := checkLimit(tier, *limit); err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		id, err := c.AddAccount(name, tier, *limit, *anchor)
		if err != nil {
			// The account name is UNIQUE in the store; surface the raw SQLite
			// constraint failure as a plain "already exists" message.
			if strings.Contains(err.Error(), "UNIQUE constraint") {
				return fmt.Errorf("an account named %q already exists", name)
			}
			return err
		}
		fmt.Fprintf(a.Stdout, "added account %q (id %d)\n", name, id)
		return nil
	})
}

// ── rm ───────────────────────────────────────────────────────────────────────

func (a *App) cmdRm(args []string) int {
	fs := a.newFlagSet("rm")
	socket := socketFlag(fs)
	cascade := fs.Bool("cascade", false, "also delete the account's ports")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 1, "rm requires an account name or id: nfuse rm <account>") {
		return 2
	}
	ref := arg(pos, 0)
	return a.withClient(*socket, func(c opClient) error {
		id, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		if err := c.DeleteAccount(id, *cascade); err != nil {
			// The daemon refuses to delete an account that still owns ports unless
			// cascade is set; point the operator at the flag it's missing.
			if !*cascade && strings.Contains(err.Error(), "still owns") {
				return fmt.Errorf("%v (pass --cascade to delete the account together with its ports)", err)
			}
			return err
		}
		fmt.Fprintf(a.Stdout, "deleted account %d\n", id)
		return nil
	})
}

// ── set-tier ─────────────────────────────────────────────────────────────────

func (a *App) cmdSetTier(args []string) int {
	fs := a.newFlagSet("set-tier")
	socket := socketFlag(fs)
	tierStr := fs.String("tier", "", "account tier: a (monthly), b (one-shot), c (unlimited)")
	limit := fs.Float64("limit", 0, "quota limit in GiB (required for tier a/b, ignored for c)")
	anchor := fs.Int("anchor", 1, "monthly billing anchor day, 1-28 (tier a)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 1, "set-tier requires an account: nfuse set-tier <account> --tier a|b|c") {
		return 2
	}
	ref := arg(pos, 0)
	tier, err := parseTier(*tierStr)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	if err := checkLimit(tier, *limit); err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		id, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		if err := c.SetTier(id, tier, *limit, *anchor); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "set account %d to tier %s\n", id, tier)
		return nil
	})
}

// ── reset ────────────────────────────────────────────────────────────────────

func (a *App) cmdReset(args []string) int {
	fs := a.newFlagSet("reset")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 1, "reset requires an account: nfuse reset <account>") {
		return 2
	}
	ref := arg(pos, 0)
	return a.withClient(*socket, func(c opClient) error {
		id, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		if err := c.ResetAccount(id); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "reset account %d\n", id)
		return nil
	})
}

// ── set-usage ────────────────────────────────────────────────────────────────

func (a *App) cmdSetUsage(args []string) int {
	fs := a.newFlagSet("set-usage")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 2, "set-usage requires an account and a byte count: nfuse set-usage <account> <bytes>") {
		return 2
	}
	ref, bytesArg := arg(pos, 0), arg(pos, 1)
	usedBytes, err := strconv.ParseUint(bytesArg, 10, 64)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: invalid byte count %q: want a non-negative integer\n", bytesArg)
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		id, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		if err := c.SetUsage(id, usedBytes); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "set account %d usage to %d bytes\n", id, usedBytes)
		return nil
	})
}

// ── persist ──────────────────────────────────────────────────────────────────

func (a *App) cmdPersist(args []string) int {
	fs := a.newFlagSet("persist")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 0, "persist takes no positional arguments: nfuse persist") {
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		if err := c.ForcePersist(); err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, "persisted")
		return nil
	})
}

// ── port ─────────────────────────────────────────────────────────────────────

func (a *App) cmdPort(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "nfuse: port requires a subcommand: add | edit | rm | move")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return a.cmdPortAdd(rest)
	case "edit":
		return a.cmdPortEdit(rest)
	case "rm":
		return a.cmdPortRm(rest)
	case "move":
		return a.cmdPortMove(rest)
	default:
		fmt.Fprintf(a.Stderr, "nfuse: unknown port subcommand %q (want: add | edit | rm | move)\n", sub)
		return 2
	}
}

func (a *App) cmdPortAdd(args []string) int {
	fs := a.newFlagSet("port add")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 2, "port add requires an account and a port: nfuse port add <account> <start[-end]>") {
		return 2
	}
	ref, spec := arg(pos, 0), arg(pos, 1)
	start, end, err := parsePortSpec(spec)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		id, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		if err := c.AddPort(id, start, end); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "added port %s to account %d\n", portSpecString(start, end), id)
		return nil
	})
}

func (a *App) cmdPortEdit(args []string) int {
	fs := a.newFlagSet("port edit")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 2, "port edit requires a port id and a port: nfuse port edit <port-id> <start[-end]>") {
		return 2
	}
	idArg, spec := arg(pos, 0), arg(pos, 1)
	portID, err := parsePortID(idArg)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	start, end, err := parsePortSpec(spec)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		if err := c.EditPort(portID, start, end); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "edited port %d to %s\n", portID, portSpecString(start, end))
		return nil
	})
}

func (a *App) cmdPortRm(args []string) int {
	fs := a.newFlagSet("port rm")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 1, "port rm requires a port id: nfuse port rm <port-id>") {
		return 2
	}
	idArg := arg(pos, 0)
	portID, err := parsePortID(idArg)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		if err := c.DeletePort(portID); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "deleted port %d\n", portID)
		return nil
	})
}

func (a *App) cmdPortMove(args []string) int {
	fs := a.newFlagSet("port move")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 2, "port move requires a port id and a target account: nfuse port move <port-id> <account>") {
		return 2
	}
	idArg, ref := arg(pos, 0), arg(pos, 1)
	portID, err := parsePortID(idArg)
	if err != nil {
		fmt.Fprintf(a.Stderr, "nfuse: %v\n", err)
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		acctID, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		if err := c.MovePort(portID, acctID); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "moved port %d to account %d\n", portID, acctID)
		return nil
	})
}

// ── token ────────────────────────────────────────────────────────────────────

func (a *App) cmdToken(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "nfuse: token requires a subcommand: show | new | master")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		return a.cmdTokenShow(rest)
	case "new":
		return a.cmdTokenNew(rest)
	case "master":
		return a.cmdTokenMaster(rest)
	default:
		fmt.Fprintf(a.Stderr, "nfuse: unknown token subcommand %q (want: show | new | master)\n", sub)
		return 2
	}
}

// cmdTokenShow prints an account's current query token. The token rides along in
// the account view, so no dedicated RPC is needed to read it.
func (a *App) cmdTokenShow(args []string) int {
	fs := a.newFlagSet("token show")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 1, "token show requires an account: nfuse token show <account>") {
		return 2
	}
	ref := arg(pos, 0)
	return a.withClient(*socket, func(c opClient) error {
		id, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		accounts, _ := c.View()
		for _, av := range accounts {
			if av.Account.ID == id {
				fmt.Fprintln(a.Stdout, av.Account.Token)
				return nil
			}
		}
		return fmt.Errorf("account %d not found", id)
	})
}

// cmdTokenNew regenerates an account's query token and prints the new value. The
// old token stops working immediately.
func (a *App) cmdTokenNew(args []string) int {
	fs := a.newFlagSet("token new")
	socket := socketFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 1, "token new requires an account: nfuse token new <account>") {
		return 2
	}
	ref := arg(pos, 0)
	return a.withClient(*socket, func(c opClient) error {
		id, err := resolveAccount(c, ref)
		if err != nil {
			return err
		}
		token, err := c.RegenerateToken(id)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, token)
		return nil
	})
}

// cmdTokenMaster prints the master query token, or regenerates it with --new.
func (a *App) cmdTokenMaster(args []string) int {
	fs := a.newFlagSet("token master")
	socket := socketFlag(fs)
	regen := fs.Bool("new", false, "regenerate the master token")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if !a.exactArgs(pos, 0, "token master takes no positional arguments: nfuse token master [--new]") {
		return 2
	}
	return a.withClient(*socket, func(c opClient) error {
		var token string
		var err error
		if *regen {
			token, err = c.RegenerateMasterToken()
		} else {
			token, err = c.MasterToken()
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, token)
		return nil
	})
}

// ── shared parsing / resolution ──────────────────────────────────────────────

// parseTier converts an a/b/c string into a model.Tier, following the model's
// own tier semantics. An empty or unknown value is an error (tier is required
// for add/set-tier).
func parseTier(s string) (model.Tier, error) {
	if s == "" {
		return "", fmt.Errorf("--tier is required (a=monthly, b=one-shot, c=unlimited)")
	}
	t := model.Tier(s)
	if !t.Valid() {
		return "", fmt.Errorf("invalid tier %q: want a (monthly), b (one-shot) or c (unlimited)", s)
	}
	return t, nil
}

// checkLimit enforces model.Validate's rule at the CLI boundary: a quota tier
// (a/b) needs a positive --limit; an unlimited tier (c) ignores it. The engine
// re-checks this, so this only exists to give a clean pre-dial error message.
func checkLimit(tier model.Tier, limit float64) error {
	if tier.HasQuota() && limit <= 0 {
		return fmt.Errorf("tier %s requires a positive --limit (in GiB)", tier)
	}
	return nil
}

// parsePortID parses a numeric port id (ports have no names, only ids).
func parsePortID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid port id %q: want the numeric id from `nfuse list`", s)
	}
	return id, nil
}

// parsePortSpec accepts either a single port ("60006") or a closed range
// ("60000-60099", with optional surrounding blanks like "60000 - 60099") and
// returns the interval. A single port yields start == end. It mirrors the TUI's
// port entry parsing so both front ends accept the same syntax.
func parsePortSpec(s string) (start, end uint16, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, fmt.Errorf("empty port")
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		lo, err1 := strconv.Atoi(strings.TrimSpace(s[:i]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if err1 != nil || err2 != nil {
			return 0, 0, fmt.Errorf("invalid port range %q", s)
		}
		if lo < 1 || hi > 65535 || lo > hi {
			return 0, 0, fmt.Errorf("port range must be 1-65535 with start ≤ end, got %q", s)
		}
		return uint16(lo), uint16(hi), nil
	}
	p, perr := strconv.Atoi(s)
	if perr != nil || p < 1 || p > 65535 {
		return 0, 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(p), uint16(p), nil
}

// portSpecString renders a [start,end] interval the way the model does.
func portSpecString(start, end uint16) string {
	return model.Port{Start: start, End: end}.String()
}

// resolveAccount turns a user-supplied account reference into an account id.
// A name takes precedence: it is looked up in the daemon's current View(). An
// ambiguous name (two accounts share it) is an error rather than a guess. Only
// when no account carries that name is the reference tried as a numeric id, and
// that id must exist. This is the rule the CLI documents: name first, numeric
// id as a fallback, never a silent guess.
func resolveAccount(c opClient, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, fmt.Errorf("empty account reference")
	}
	accounts, statusErr := c.View()
	if accounts == nil && statusErr != "" {
		return 0, fmt.Errorf("cannot read account list: %s", statusErr)
	}

	var matches []int64
	for _, av := range accounts {
		if av.Account.Name == ref {
			matches = append(matches, av.Account.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return 0, fmt.Errorf("account name %q is ambiguous (%d accounts share it); use the numeric id", ref, len(matches))
	}

	// No name match: try a numeric id, and require that it exists.
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		for _, av := range accounts {
			if av.Account.ID == id {
				return id, nil
			}
		}
		return 0, fmt.Errorf("no account with id %d", id)
	}
	return 0, fmt.Errorf("no account named %q", ref)
}

// ── output rendering ─────────────────────────────────────────────────────────

// jsonPort is the stable JSON shape of one metered port.
type jsonPort struct {
	ID    int64  `json:"id"`
	Start uint16 `json:"start"`
	End   uint16 `json:"end"`
}

// jsonAccount is the stable JSON shape of one account, mirroring the fields of
// engine.AccountView. Usage is reported as raw bytes so scripts do the math;
// the limit is reported in GiB (the model's own unit) and as bytes.
type jsonAccount struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Tier       string     `json:"tier"`
	LimitGiB   float64    `json:"limit_gib"`
	LimitBytes uint64     `json:"limit_bytes"`
	UsedBytes  uint64     `json:"used_bytes"`
	Ports      []jsonPort `json:"ports"`
}

// writeJSON emits the account list as stable JSON on stdout. An empty list is
// rendered as `[]` (never null), and each account's ports likewise default to
// `[]`, so downstream parsers see a consistent shape.
func (a *App) writeJSON(accounts []engine.AccountView) error {
	out := make([]jsonAccount, 0, len(accounts))
	for _, av := range accounts {
		ports := make([]jsonPort, 0, len(av.Ports))
		for _, p := range av.Ports {
			ports = append(ports, jsonPort{ID: p.PortID, Start: p.Start, End: p.End})
		}
		out = append(out, jsonAccount{
			ID:         av.Account.ID,
			Name:       av.Account.Name,
			Tier:       string(av.Account.Tier),
			LimitGiB:   av.Account.LimitGiB,
			LimitBytes: av.Account.LimitBytes(),
			UsedBytes:  av.UsedBytes,
			Ports:      ports,
		})
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// writeHuman renders the account list as a human-readable report on stdout,
// showing each port's id so `port edit/rm/move` have a handle to target.
func (a *App) writeHuman(accounts []engine.AccountView) {
	if len(accounts) == 0 {
		fmt.Fprintln(a.Stdout, "(no accounts)")
		return
	}
	for _, av := range accounts {
		acct := av.Account
		fmt.Fprintf(a.Stdout, "#%d %s  [%s]", acct.ID, acct.Name, acct.Tier.Describe())
		if acct.Tier.HasQuota() {
			fmt.Fprintf(a.Stdout, "  used %s / %s", model.FormatBytes(av.UsedBytes), model.FormatBytes(acct.LimitBytes()))
			if acct.Tier.Resets() {
				fmt.Fprintf(a.Stdout, "  anchor day %d", acct.BillingAnchorDay)
			}
			if acct.Breached() {
				fmt.Fprint(a.Stdout, "  BREACHED")
			}
		} else {
			fmt.Fprintf(a.Stdout, "  used %s (unlimited)", model.FormatBytes(av.UsedBytes))
		}
		fmt.Fprintln(a.Stdout)

		if len(av.Ports) == 0 {
			fmt.Fprintln(a.Stdout, "    (no ports)")
			continue
		}
		for _, p := range av.Ports {
			fmt.Fprintf(a.Stdout, "    port #%d  %s  in %s  out %s\n",
				p.PortID, portSpecString(p.Start, p.End),
				model.FormatBytes(p.InBytes), model.FormatBytes(p.OutBytes))
		}
	}
}
