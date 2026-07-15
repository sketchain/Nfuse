// Package httpapi serves read-only usage queries over HTTP for the nfuse daemon.
//
// It exists so an operator (or a billing script) can read an account's metered
// usage with a plain `curl <host:port>/<token>` — no Unix socket, no client
// binary. It is intentionally minimal and read-only: it never mutates state, and
// it is started *only* by the `nfuse server` daemon role (never by `nfuse tui`
// or the operational commands), bound to 127.0.0.1 by default.
//
// A request path carries exactly one token:
//
//	GET /<token>            → that account's usage (a per-user account token) or
//	                          every account's usage (the master token)
//	GET /<token>?format=json  machine-readable JSON (default is plain text)
//
// The token is looked up against the engine; an unknown or empty token is 404.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// Backend is the engine surface the query server needs: resolve a token to the
// account views it grants access to. Satisfied by *engine.Controller.
type Backend interface {
	QueryByToken(token string) (views []engine.AccountView, all bool, ok bool)
}

// Server is the HTTP query endpoint. It wraps a net/http server so the daemon can
// start it (Listen+Serve) and stop it gracefully (Close) alongside the RPC socket.
type Server struct {
	be   Backend
	logf func(string, ...any)
	ln   net.Listener
	srv  *http.Server
}

// New builds a query Server over the given backend. logf may be nil.
func New(be Backend, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{be: be, logf: logf}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Listen binds the TCP address (e.g. "127.0.0.1:8787" or "0.0.0.0:8787"). It is
// split from Serve so the daemon can report a bind failure synchronously and
// exit, rather than discovering it on a background goroutine.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.ln = ln
	return nil
}

// Addr returns the actual bound address (useful when the port was chosen as :0).
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Serve accepts connections until Close is called. It blocks; run it in a
// goroutine. A clean shutdown via Close returns nil.
func (s *Server) Serve() error {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close shuts the server down gracefully.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// handle answers one query request. Only GET/HEAD are allowed; the path's first
// segment is the token; the optional ?format= (or bare ?json) selects the output.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.Trim(r.URL.Path, "/")
	// Only the first path segment is the token; ignore any trailing path so a
	// stray slash doesn't turn a valid token into an unknown one.
	if i := strings.IndexByte(token, '/'); i >= 0 {
		token = token[:i]
	}
	if token == "" {
		http.Error(w, "usage: curl <host:port>/<token>[?format=json]", http.StatusNotFound)
		return
	}
	views, _, ok := s.be.QueryByToken(token)
	if !ok {
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}
	switch queryFormat(r) {
	case "json":
		s.writeJSON(w, views)
	default:
		s.writeText(w, views)
	}
}

// queryFormat picks the output format from the query string: ?format=json|text,
// or the bare ?json shorthand. Anything else falls back to text.
func queryFormat(r *http.Request) string {
	q := r.URL.Query()
	if f := q.Get("format"); f != "" {
		return strings.ToLower(f)
	}
	if _, ok := q["json"]; ok {
		return "json"
	}
	return "text"
}

// portJSON / acctJSON are the stable JSON shapes for a query response. Tokens are
// deliberately never included in a query response.
type portJSON struct {
	Start    uint16 `json:"start"`
	End      uint16 `json:"end"`
	InBytes  uint64 `json:"in_bytes"`
	OutBytes uint64 `json:"out_bytes"`
}

type acctJSON struct {
	Name       string     `json:"name"`
	Tier       string     `json:"tier"`
	LimitGiB   float64    `json:"limit_gib"`
	LimitBytes uint64     `json:"limit_bytes"`
	UsedBytes  uint64     `json:"used_bytes"`
	Breached   bool       `json:"breached"`
	Ports      []portJSON `json:"ports"`
}

// writeJSON emits the views as a JSON array (always an array, even for a single
// account token, so scripts see one consistent shape). Empty port lists render
// as [], never null.
func (s *Server) writeJSON(w http.ResponseWriter, views []engine.AccountView) {
	out := make([]acctJSON, 0, len(views))
	for _, av := range views {
		ports := make([]portJSON, 0, len(av.Ports))
		for _, p := range av.Ports {
			ports = append(ports, portJSON{Start: p.Start, End: p.End, InBytes: p.InBytes, OutBytes: p.OutBytes})
		}
		out = append(out, acctJSON{
			Name:       av.Account.Name,
			Tier:       string(av.Account.Tier),
			LimitGiB:   av.Account.LimitGiB,
			LimitBytes: av.Account.LimitBytes(),
			UsedBytes:  av.UsedBytes,
			Breached:   breached(av),
			Ports:      ports,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		s.logf("http query: write json: %v", err)
	}
}

// writeText renders the views as a compact human-readable report, one account
// per block with its ports indented beneath it.
func (s *Server) writeText(w http.ResponseWriter, views []engine.AccountView) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var b strings.Builder
	for _, av := range views {
		a := av.Account
		if a.Tier.HasQuota() {
			var pct float64
			if lb := a.LimitBytes(); lb > 0 {
				pct = float64(av.UsedBytes) / float64(lb) * 100
			}
			fmt.Fprintf(&b, "%s  [%s]  used %s / %s (%.1f%%)",
				a.Name, a.Tier.Describe(),
				model.FormatBytes(av.UsedBytes), model.FormatBytes(a.LimitBytes()), pct)
			if breached(av) {
				b.WriteString("  BREACHED")
			}
		} else {
			fmt.Fprintf(&b, "%s  [%s]  used %s", a.Name, a.Tier.Describe(), model.FormatBytes(av.UsedBytes))
		}
		b.WriteByte('\n')
		for _, p := range av.Ports {
			fmt.Fprintf(&b, "  port %s  in %s  out %s\n",
				model.Port{Start: p.Start, End: p.End}.String(),
				model.FormatBytes(p.InBytes), model.FormatBytes(p.OutBytes))
		}
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		s.logf("http query: write text: %v", err)
	}
}

// breached reports whether the account is at/over its quota, computed from the
// *live* used bytes in the view (not the account's persisted snapshot).
func breached(av engine.AccountView) bool {
	return av.Account.Tier.HasQuota() && av.UsedBytes >= av.Account.LimitBytes()
}
