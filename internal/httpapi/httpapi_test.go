package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// fakeBackend maps tokens to views, mimicking engine.QueryByToken without an
// engine or kernel.
type fakeBackend struct {
	master string
	byTok  map[string]engine.AccountView
	all    []engine.AccountView
}

func (f *fakeBackend) QueryByToken(token string) ([]engine.AccountView, bool, bool) {
	if token == "" {
		return nil, false, false
	}
	if token == f.master {
		return f.all, true, true
	}
	if av, ok := f.byTok[token]; ok {
		return []engine.AccountView{av}, false, true
	}
	return nil, false, false
}

func newBackend() *fakeBackend {
	alice := engine.AccountView{
		Account:   model.Account{ID: 1, Name: "alice", Tier: model.TierMonthly, LimitGiB: 10},
		UsedBytes: 5 * (1 << 30),
		Ports: []engine.PortView{
			{PortID: 11, Start: 60000, End: 60099, InBytes: 1 << 30, OutBytes: 4 * (1 << 30)},
		},
	}
	bob := engine.AccountView{
		Account:   model.Account{ID: 2, Name: "bob", Tier: model.TierUnlimited},
		UsedBytes: 500,
	}
	return &fakeBackend{
		master: "MASTERtokenABCDEF",
		byTok: map[string]engine.AccountView{
			"aliceTokenABCDEFG": alice,
			"bobTokenABCDEFGHI": bob,
		},
		all: []engine.AccountView{alice, bob},
	}
}

// do runs one request against a Server built on the given backend.
func do(t *testing.T, be Backend, method, target string) *http.Response {
	t.Helper()
	s := New(be, t.Logf)
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	return rec.Result()
}

func TestQueryAccountTokenText(t *testing.T) {
	resp := do(t, newBackend(), http.MethodGet, "/aliceTokenABCDEFG")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "alice") || !strings.Contains(body, "60000-60099") {
		t.Fatalf("text body missing account/port info:\n%s", body)
	}
	if strings.Contains(body, "bob") {
		t.Fatalf("account token leaked another account:\n%s", body)
	}
}

func TestQueryAccountTokenJSON(t *testing.T) {
	resp := do(t, newBackend(), http.MethodGet, "/aliceTokenABCDEFG?format=json")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want json", ct)
	}
	var got []acctJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("json = %+v, want one alice", got)
	}
	if got[0].UsedBytes != 5*(1<<30) || got[0].LimitBytes != 10*(1<<30) {
		t.Fatalf("json usage/limit = %d/%d", got[0].UsedBytes, got[0].LimitBytes)
	}
	if len(got[0].Ports) != 1 || got[0].Ports[0].Start != 60000 {
		t.Fatalf("json ports = %+v", got[0].Ports)
	}
	// A token must never be echoed back in the response.
	if strings.Contains(readBodyFrom(t, got), "Token") {
		t.Fatalf("token field leaked into JSON")
	}
}

// The bare ?json shorthand selects JSON too.
func TestQueryJSONShorthand(t *testing.T) {
	resp := do(t, newBackend(), http.MethodGet, "/aliceTokenABCDEFG?json")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("?json shorthand content-type = %q, want json", ct)
	}
}

func TestQueryMasterTokenReturnsAll(t *testing.T) {
	resp := do(t, newBackend(), http.MethodGet, "/MASTERtokenABCDEF?format=json")
	var got []acctJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("master query returned %d accounts, want 2", len(got))
	}
}

func TestUnknownTokenIs404(t *testing.T) {
	resp := do(t, newBackend(), http.MethodGet, "/doesNotExist12345")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown token status = %d, want 404", resp.StatusCode)
	}
}

func TestEmptyPathIs404(t *testing.T) {
	resp := do(t, newBackend(), http.MethodGet, "/")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("empty path status = %d, want 404", resp.StatusCode)
	}
}

func TestTrailingPathIgnored(t *testing.T) {
	resp := do(t, newBackend(), http.MethodGet, "/aliceTokenABCDEFG/extra/junk")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trailing path status = %d, want 200 (only first segment is the token)", resp.StatusCode)
	}
}

func TestNonGetRejected(t *testing.T) {
	resp := do(t, newBackend(), http.MethodPost, "/aliceTokenABCDEFG")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

// readBodyFrom re-marshals decoded accounts so the leak check operates on the
// canonical JSON field names actually emitted.
func readBodyFrom(t *testing.T, v []acctJSON) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
