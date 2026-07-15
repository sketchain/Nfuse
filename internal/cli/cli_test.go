package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// fakeClient is an in-memory opClient that records the calls the CLI makes and
// returns canned account state, so the operational commands can be exercised
// without a live daemon.
type fakeClient struct {
	accounts []engine.AccountView
	viewErr  string

	// Recorded calls.
	added       []addCall
	deleted     []deleteCall
	setTiers    []setTierCall
	addedPorts  []addPortCall
	editPorts   []editPortCall
	delPorts    []int64
	movePorts   []movePortCall
	resets      []int64
	setUsages   []setUsageCall
	persisted   int
	regenTokens []int64
	regenMaster int

	// Optional canned error for the next mutation.
	mutErr error

	closed bool
}

type addCall struct {
	name   string
	tier   model.Tier
	limit  float64
	anchor int
}
type deleteCall struct {
	id      int64
	cascade bool
}
type setTierCall struct {
	id     int64
	tier   model.Tier
	limit  float64
	anchor int
}
type addPortCall struct {
	acctID     int64
	start, end uint16
}
type editPortCall struct {
	portID     int64
	start, end uint16
}
type movePortCall struct {
	portID, acctID int64
}
type setUsageCall struct {
	id    int64
	bytes uint64
}

func (f *fakeClient) View() ([]engine.AccountView, string) { return f.accounts, f.viewErr }
func (f *fakeClient) AddAccount(name string, tier model.Tier, limitGiB float64, anchorDay int) (int64, error) {
	if f.mutErr != nil {
		return 0, f.mutErr
	}
	f.added = append(f.added, addCall{name, tier, limitGiB, anchorDay})
	return 42, nil
}
func (f *fakeClient) DeleteAccount(id int64, cascade bool) error {
	f.deleted = append(f.deleted, deleteCall{id, cascade})
	return f.mutErr
}
func (f *fakeClient) SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error {
	f.setTiers = append(f.setTiers, setTierCall{id, tier, limitGiB, anchorDay})
	return f.mutErr
}
func (f *fakeClient) AddPort(accountID int64, start, end uint16) error {
	f.addedPorts = append(f.addedPorts, addPortCall{accountID, start, end})
	return f.mutErr
}
func (f *fakeClient) EditPort(portID int64, start, end uint16) error {
	f.editPorts = append(f.editPorts, editPortCall{portID, start, end})
	return f.mutErr
}
func (f *fakeClient) DeletePort(portID int64) error {
	f.delPorts = append(f.delPorts, portID)
	return f.mutErr
}
func (f *fakeClient) MovePort(portID, newAccountID int64) error {
	f.movePorts = append(f.movePorts, movePortCall{portID, newAccountID})
	return f.mutErr
}
func (f *fakeClient) ResetAccount(id int64) error {
	f.resets = append(f.resets, id)
	return f.mutErr
}
func (f *fakeClient) SetUsage(id int64, usedBytes uint64) error {
	f.setUsages = append(f.setUsages, setUsageCall{id, usedBytes})
	return f.mutErr
}
func (f *fakeClient) ForcePersist() error {
	f.persisted++
	return f.mutErr
}
func (f *fakeClient) RegenerateToken(id int64) (string, error) {
	if f.mutErr != nil {
		return "", f.mutErr
	}
	f.regenTokens = append(f.regenTokens, id)
	return "NewTokenAbcdEfgh", nil
}
func (f *fakeClient) MasterToken() (string, error) {
	if f.mutErr != nil {
		return "", f.mutErr
	}
	return "MasterTokenAbcdEf", nil
}
func (f *fakeClient) RegenerateMasterToken() (string, error) {
	if f.mutErr != nil {
		return "", f.mutErr
	}
	f.regenMaster++
	return "MasterTokenNewAbc", nil
}
func (f *fakeClient) Close() error { f.closed = true; return nil }

// newTestApp returns an App wired to the given fake client and captures its
// output streams.
func newTestApp(fc *fakeClient) (*App, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	app := &App{
		Stdout: &out,
		Stderr: &errb,
		Dial:   func(string) (opClient, error) { return fc, nil },
		Build:  BuildInfo{Version: "test", Commit: "abc", Date: "today"},
	}
	return app, &out, &errb
}

func sampleAccounts() []engine.AccountView {
	return []engine.AccountView{
		{
			Account:   model.Account{ID: 1, Name: "alice", Tier: model.TierMonthly, LimitGiB: 10, BillingAnchorDay: 5},
			UsedBytes: 1 << 30,
			Ports: []engine.PortView{
				{PortID: 11, Start: 60000, End: 60099, InBytes: 100, OutBytes: 200},
				{PortID: 12, Start: 8080, End: 8080},
			},
		},
		{
			Account:   model.Account{ID: 2, Name: "bob", Tier: model.TierUnlimited},
			UsedBytes: 500,
		},
	}
}

// ── dispatch ─────────────────────────────────────────────────────────────────

func TestRunNoArgsPrintsUsageNonZero(t *testing.T) {
	app, _, errb := newTestApp(&fakeClient{})
	if code := app.Run(nil); code != 2 {
		t.Fatalf("bare nfuse exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Usage:") {
		t.Errorf("expected usage on stderr, got %q", errb.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	app, _, errb := newTestApp(&fakeClient{})
	if code := app.Run([]string{"frobnicate"}); code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("expected unknown-command message, got %q", errb.String())
	}
}

func TestRunVersion(t *testing.T) {
	app, out, _ := newTestApp(&fakeClient{})
	if code := app.Run([]string{"version"}); code != 0 {
		t.Fatalf("version exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "nfuse test") {
		t.Errorf("version output = %q, want it to contain the version", out.String())
	}
}

// TestDispatchRoutesOperations checks each operational verb reaches its client
// method (dispatch wiring), using references that resolve cleanly.
func TestDispatchRoutesOperations(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		check func(t *testing.T, fc *fakeClient)
	}{
		{"add", []string{"add", "carol", "--tier", "c"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.added) != 1 || fc.added[0].name != "carol" || fc.added[0].tier != model.TierUnlimited {
				t.Errorf("add not routed: %+v", fc.added)
			}
		}},
		{"rm", []string{"rm", "alice"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.deleted) != 1 || fc.deleted[0].id != 1 {
				t.Errorf("rm not routed: %+v", fc.deleted)
			}
		}},
		{"set-tier", []string{"set-tier", "2", "--tier", "a", "--limit", "5"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.setTiers) != 1 || fc.setTiers[0].id != 2 || fc.setTiers[0].tier != model.TierMonthly {
				t.Errorf("set-tier not routed: %+v", fc.setTiers)
			}
		}},
		{"reset", []string{"reset", "bob"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.resets) != 1 || fc.resets[0] != 2 {
				t.Errorf("reset not routed: %+v", fc.resets)
			}
		}},
		{"set-usage", []string{"set-usage", "alice", "12345"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.setUsages) != 1 || fc.setUsages[0].bytes != 12345 {
				t.Errorf("set-usage not routed: %+v", fc.setUsages)
			}
		}},
		{"persist", []string{"persist"}, func(t *testing.T, fc *fakeClient) {
			if fc.persisted != 1 {
				t.Errorf("persist not routed: %d", fc.persisted)
			}
		}},
		{"port add", []string{"port", "add", "alice", "9000-9100"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.addedPorts) != 1 || fc.addedPorts[0].start != 9000 || fc.addedPorts[0].end != 9100 {
				t.Errorf("port add not routed: %+v", fc.addedPorts)
			}
		}},
		{"port edit", []string{"port", "edit", "11", "7000"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.editPorts) != 1 || fc.editPorts[0].portID != 11 || fc.editPorts[0].start != 7000 {
				t.Errorf("port edit not routed: %+v", fc.editPorts)
			}
		}},
		{"port rm", []string{"port", "rm", "12"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.delPorts) != 1 || fc.delPorts[0] != 12 {
				t.Errorf("port rm not routed: %+v", fc.delPorts)
			}
		}},
		{"port move", []string{"port", "move", "11", "bob"}, func(t *testing.T, fc *fakeClient) {
			if len(fc.movePorts) != 1 || fc.movePorts[0].portID != 11 || fc.movePorts[0].acctID != 2 {
				t.Errorf("port move not routed: %+v", fc.movePorts)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{accounts: sampleAccounts()}
			app, _, errb := newTestApp(fc)
			if code := app.Run(tc.args); code != 0 {
				t.Fatalf("%s exit = %d (stderr %q), want 0", tc.name, code, errb.String())
			}
			if !fc.closed {
				t.Errorf("%s: client was not closed", tc.name)
			}
			tc.check(t, fc)
		})
	}
}

// ── token commands ───────────────────────────────────────────────────────────

func TestTokenShow(t *testing.T) {
	accts := sampleAccounts()
	accts[0].Account.Token = "aliceTokenABCDEFG"
	fc := &fakeClient{accounts: accts}
	app, out, errb := newTestApp(fc)
	if code := app.Run([]string{"token", "show", "alice"}); code != 0 {
		t.Fatalf("token show exit = %d (stderr %q), want 0", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "aliceTokenABCDEFG" {
		t.Errorf("token show output = %q, want the account token", out.String())
	}
}

func TestTokenNew(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, out, errb := newTestApp(fc)
	if code := app.Run([]string{"token", "new", "alice"}); code != 0 {
		t.Fatalf("token new exit = %d (stderr %q), want 0", code, errb.String())
	}
	if len(fc.regenTokens) != 1 || fc.regenTokens[0] != 1 {
		t.Errorf("token new not routed to account 1: %+v", fc.regenTokens)
	}
	if strings.TrimSpace(out.String()) != "NewTokenAbcdEfgh" {
		t.Errorf("token new output = %q, want the fresh token", out.String())
	}
}

func TestTokenMasterShow(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, out, _ := newTestApp(fc)
	if code := app.Run([]string{"token", "master"}); code != 0 {
		t.Fatalf("token master exit = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) != "MasterTokenAbcdEf" {
		t.Errorf("token master output = %q, want the master token", out.String())
	}
	if fc.regenMaster != 0 {
		t.Errorf("token master without --new must not regenerate")
	}
}

func TestTokenMasterNew(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, out, _ := newTestApp(fc)
	if code := app.Run([]string{"token", "master", "--new"}); code != 0 {
		t.Fatalf("token master --new exit = %d, want 0", code)
	}
	if fc.regenMaster != 1 {
		t.Errorf("token master --new must regenerate, got %d", fc.regenMaster)
	}
	if strings.TrimSpace(out.String()) != "MasterTokenNewAbc" {
		t.Errorf("token master --new output = %q", out.String())
	}
}

func TestTokenUnknownSub(t *testing.T) {
	fc := &fakeClient{}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"token", "frob"}); code != 2 {
		t.Fatalf("unknown token sub exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown token subcommand") {
		t.Errorf("expected unknown-subcommand error, got %q", errb.String())
	}
}

// ── account resolution ───────────────────────────────────────────────────────

func TestResolveAccountByName(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	id, err := resolveAccount(fc, "bob")
	if err != nil || id != 2 {
		t.Fatalf("resolveAccount(bob) = %d, %v; want 2, nil", id, err)
	}
}

func TestResolveAccountByID(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	id, err := resolveAccount(fc, "1")
	if err != nil || id != 1 {
		t.Fatalf("resolveAccount(1) = %d, %v; want 1, nil", id, err)
	}
}

func TestResolveAccountAmbiguousName(t *testing.T) {
	accts := sampleAccounts()
	accts = append(accts, engine.AccountView{
		Account: model.Account{ID: 3, Name: "alice", Tier: model.TierUnlimited},
	})
	fc := &fakeClient{accounts: accts}
	if _, err := resolveAccount(fc, "alice"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("resolveAccount(ambiguous alice) err = %v, want ambiguity error", err)
	}
}

func TestResolveAccountUnknownName(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	if _, err := resolveAccount(fc, "nobody"); err == nil || !strings.Contains(err.Error(), "no account named") {
		t.Fatalf("resolveAccount(nobody) err = %v, want not-found", err)
	}
}

func TestResolveAccountUnknownID(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	if _, err := resolveAccount(fc, "999"); err == nil || !strings.Contains(err.Error(), "no account with id") {
		t.Fatalf("resolveAccount(999) err = %v, want id-not-found", err)
	}
}

// TestResolveNamePrecedenceOverNumericName: a purely numeric account *name*
// resolves by name before the numeric-id fallback is tried.
func TestResolveNamePrecedenceOverNumericName(t *testing.T) {
	fc := &fakeClient{accounts: []engine.AccountView{
		{Account: model.Account{ID: 7, Name: "100", Tier: model.TierUnlimited}},
		{Account: model.Account{ID: 100, Name: "real", Tier: model.TierUnlimited}},
	}}
	id, err := resolveAccount(fc, "100")
	if err != nil || id != 7 {
		t.Fatalf("resolveAccount(100) = %d, %v; want 7 (name match wins), nil", id, err)
	}
}

func TestRmUnknownAccountExitsNonZero(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"rm", "ghost"}); code != 1 {
		t.Fatalf("rm ghost exit = %d, want 1", code)
	}
	if len(fc.deleted) != 0 {
		t.Errorf("rm should not have called DeleteAccount for an unresolved ref")
	}
	if !strings.Contains(errb.String(), "no account named") {
		t.Errorf("expected not-found on stderr, got %q", errb.String())
	}
}

// ── error-message wrapping ───────────────────────────────────────────────────

// TestAddDuplicateNameWrapped: the raw SQLite UNIQUE constraint failure is
// rewritten into a plain "already exists" message at the CLI boundary.
func TestAddDuplicateNameWrapped(t *testing.T) {
	fc := &fakeClient{mutErr: fmt.Errorf("constraint failed: UNIQUE constraint failed: accounts.name (2067)")}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"add", "alice", "--tier", "c"}); code != 1 {
		t.Fatalf("duplicate add exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), `account named "alice" already exists`) {
		t.Errorf("expected friendly duplicate message, got %q", errb.String())
	}
	if strings.Contains(errb.String(), "UNIQUE") {
		t.Errorf("raw SQLite error should not leak to the user, got %q", errb.String())
	}
}

// TestRmPortsOwnedHintsCascade: rm of an account that still owns ports appends a
// --cascade hint to the daemon's refusal.
func TestRmPortsOwnedHintsCascade(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts(), mutErr: fmt.Errorf("account still owns 2 port(s); remove them first")}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"rm", "alice"}); code != 1 {
		t.Fatalf("rm with ports exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "--cascade") {
		t.Errorf("expected a --cascade hint, got %q", errb.String())
	}
}

// TestRmCascadeSuppressesHint: with --cascade already set, no hint is appended
// (the error, if any, passes through unchanged).
func TestRmCascadeSuppressesHint(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts(), mutErr: fmt.Errorf("account still owns 2 port(s); remove them first")}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"rm", "alice", "--cascade"}); code != 1 {
		t.Fatalf("rm --cascade exit = %d, want 1", code)
	}
	if strings.Contains(errb.String(), "pass --cascade") {
		t.Errorf("should not hint --cascade when it is already set, got %q", errb.String())
	}
}

// ── required-argument error paths ────────────────────────────────────────────

func TestAddMissingTier(t *testing.T) {
	fc := &fakeClient{}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"add", "dave"}); code != 2 {
		t.Fatalf("add without --tier exit = %d, want 2", code)
	}
	if len(fc.added) != 0 {
		t.Errorf("add must not reach the daemon without a tier")
	}
	if !strings.Contains(errb.String(), "--tier is required") {
		t.Errorf("expected --tier required message, got %q", errb.String())
	}
}

func TestAddQuotaTierMissingLimit(t *testing.T) {
	for _, tier := range []string{"a", "b"} {
		fc := &fakeClient{}
		app, _, errb := newTestApp(fc)
		code := app.Run([]string{"add", "dave", "--tier", tier})
		if code != 2 {
			t.Fatalf("tier %s without --limit exit = %d, want 2", tier, code)
		}
		if len(fc.added) != 0 {
			t.Errorf("tier %s: add must not reach daemon without a limit", tier)
		}
		if !strings.Contains(errb.String(), "positive --limit") {
			t.Errorf("tier %s: expected positive-limit message, got %q", tier, errb.String())
		}
	}
}

func TestAddUnlimitedIgnoresLimit(t *testing.T) {
	fc := &fakeClient{}
	app, _, _ := newTestApp(fc)
	if code := app.Run([]string{"add", "dave", "--tier", "c"}); code != 0 {
		t.Fatalf("add tier c exit = %d, want 0", code)
	}
	if len(fc.added) != 1 || fc.added[0].limit != 0 {
		t.Errorf("tier c add = %+v, want one call with limit 0", fc.added)
	}
}

func TestAddInvalidTier(t *testing.T) {
	fc := &fakeClient{}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"add", "dave", "--tier", "z"}); code != 2 {
		t.Fatalf("invalid tier exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "invalid tier") {
		t.Errorf("expected invalid-tier message, got %q", errb.String())
	}
}

func TestSetUsageBadBytes(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"set-usage", "alice", "notanumber"}); code != 2 {
		t.Fatalf("set-usage bad bytes exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "invalid byte count") {
		t.Errorf("expected invalid byte count, got %q", errb.String())
	}
}

func TestPortEditBadID(t *testing.T) {
	fc := &fakeClient{}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"port", "edit", "abc", "7000"}); code != 2 {
		t.Fatalf("port edit bad id exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "invalid port id") {
		t.Errorf("expected invalid port id, got %q", errb.String())
	}
}

func TestPortAddBadSpec(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, _, errb := newTestApp(fc)
	if code := app.Run([]string{"port", "add", "alice", "70000"}); code != 2 {
		t.Fatalf("port add bad spec exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "invalid port") {
		t.Errorf("expected invalid port, got %q", errb.String())
	}
}

// ── surplus positional arguments ─────────────────────────────────────────────

// TestExtraPositionalArgsRejected covers every operational subcommand: a token
// beyond its fixed arity must be an error (exit 2) rather than silently dropped,
// and the daemon must not be touched. `otherwise good` args are chosen so the
// only fault is the trailing extra token.
func TestExtraPositionalArgsRejected(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"list", []string{"list", "bogus"}},
		{"list --json", []string{"list", "--json", "bogus"}},
		{"persist", []string{"persist", "bogus"}},
		{"add", []string{"add", "carol", "--tier", "c", "bogus"}},
		{"rm", []string{"rm", "alice", "bogus"}},
		{"set-tier", []string{"set-tier", "alice", "--tier", "c", "bogus"}},
		{"reset", []string{"reset", "alice", "bogus"}},
		{"set-usage", []string{"set-usage", "alice", "100", "bogus"}},
		{"port add", []string{"port", "add", "alice", "9000", "bogus"}},
		{"port edit", []string{"port", "edit", "11", "9000", "bogus"}},
		{"port rm", []string{"port", "rm", "11", "bogus"}},
		{"port move", []string{"port", "move", "11", "bob", "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{accounts: sampleAccounts()}
			app, _, errb := newTestApp(fc)
			if code := app.Run(tc.args); code != 2 {
				t.Fatalf("%s with extra arg exit = %d, want 2", tc.name, code)
			}
			if !strings.Contains(errb.String(), "too many arguments") {
				t.Errorf("%s: expected too-many-arguments error, got %q", tc.name, errb.String())
			}
			if fc.added != nil || fc.deleted != nil || fc.setTiers != nil ||
				fc.addedPorts != nil || fc.editPorts != nil || fc.delPorts != nil ||
				fc.movePorts != nil || fc.resets != nil || fc.setUsages != nil || fc.persisted != 0 {
				t.Errorf("%s: a surplus-argument command must not reach the daemon", tc.name)
			}
		})
	}
}

// ── --json output ────────────────────────────────────────────────────────────

func TestListJSONRoundTrips(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, out, _ := newTestApp(fc)
	if code := app.Run([]string{"list", "--json"}); code != 0 {
		t.Fatalf("list --json exit = %d, want 0", code)
	}
	var got []jsonAccount
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2", len(got))
	}
	a0 := got[0]
	if a0.ID != 1 || a0.Name != "alice" || a0.Tier != "a" {
		t.Errorf("account[0] = %+v, want alice/a", a0)
	}
	if a0.UsedBytes != 1<<30 {
		t.Errorf("used_bytes = %d, want %d (raw bytes)", a0.UsedBytes, uint64(1<<30))
	}
	if a0.LimitBytes != 10*(1<<30) {
		t.Errorf("limit_bytes = %d, want %d", a0.LimitBytes, uint64(10*(1<<30)))
	}
	if len(a0.Ports) != 2 || a0.Ports[0].ID != 11 || a0.Ports[0].Start != 60000 || a0.Ports[0].End != 60099 {
		t.Errorf("account[0].ports = %+v, want port id 11 spanning 60000-60099", a0.Ports)
	}
	// Unlimited account carries no ports; must still be an empty array, not null.
	if got[1].Ports == nil {
		t.Errorf("empty port list decoded as nil; JSON should emit []")
	}
}

func TestListJSONEmptyIsArray(t *testing.T) {
	fc := &fakeClient{accounts: []engine.AccountView{}}
	app, out, _ := newTestApp(fc)
	if code := app.Run([]string{"list", "--json"}); code != 0 {
		t.Fatalf("list --json exit = %d, want 0", code)
	}
	trimmed := strings.TrimSpace(out.String())
	if trimmed != "[]" {
		t.Errorf("empty list --json = %q, want []", trimmed)
	}
}

// TestListJSONStdoutStderrSeparation: a last-sampling warning goes to stderr
// while stdout stays pure JSON, so a piped `| jq` is never corrupted.
func TestListJSONStdoutStderrSeparation(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts(), viewErr: "sample failed: timeout"}
	app, out, errb := newTestApp(fc)
	if code := app.Run([]string{"list", "--json"}); code != 0 {
		t.Fatalf("list --json exit = %d, want 0", code)
	}
	if err := json.Unmarshal(out.Bytes(), &[]jsonAccount{}); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\n%s", err, out.String())
	}
	if !strings.Contains(errb.String(), "sample failed") {
		t.Errorf("sampling warning should be on stderr, got %q", errb.String())
	}
}

func TestListHumanShowsPortIDs(t *testing.T) {
	fc := &fakeClient{accounts: sampleAccounts()}
	app, out, _ := newTestApp(fc)
	if code := app.Run([]string{"list"}); code != 0 {
		t.Fatalf("list exit = %d, want 0", code)
	}
	s := out.String()
	if !strings.Contains(s, "port #11") || !strings.Contains(s, "60000-60099") {
		t.Errorf("human list should show port ids and ranges, got:\n%s", s)
	}
	if !strings.Contains(s, "alice") || !strings.Contains(s, "unlimited") {
		t.Errorf("human list missing expected account info:\n%s", s)
	}
}

// ── dial failure ─────────────────────────────────────────────────────────────

func TestListDialFailure(t *testing.T) {
	var out, errb bytes.Buffer
	app := &App{
		Stdout: &out,
		Stderr: &errb,
		Dial:   func(string) (opClient, error) { return nil, fmt.Errorf("connection refused") },
	}
	if code := app.Run([]string{"list"}); code != 1 {
		t.Fatalf("list with dead daemon exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "cannot connect") {
		t.Errorf("expected connect error on stderr, got %q", errb.String())
	}
}

// ── lifecycle helpers ────────────────────────────────────────────────────────

func TestValidateIface(t *testing.T) {
	if err := validateIface(""); err == nil {
		t.Error("empty --iface must be rejected for the server role")
	} else if !strings.Contains(err.Error(), "--iface") {
		t.Errorf("empty-iface error should mention --iface: %v", err)
	}
	if err := validateIface("definitely-not-a-real-nic0"); err == nil {
		t.Error("non-existent --iface must be rejected")
	}
	// Loopback is always present: the daemon role must accept a real interface.
	if _, err := net.InterfaceByName("lo"); err == nil {
		if err := validateIface("lo"); err != nil {
			t.Errorf("validateIface(lo) = %v, want nil", err)
		}
	}
}
