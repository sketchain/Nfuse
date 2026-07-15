package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// Backend is the engine surface the server exposes over the socket. It is
// satisfied by *engine.Controller; defining it as an interface keeps the server
// testable without a live kernel.
type Backend interface {
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
	MasterToken() string
	RegenerateMasterToken() (string, error)
	Stats() (startedAt, lastPersist time.Time)
}

// Server serves RPCs for one Backend over a Unix domain socket.
//
// The engine already serializes every mutation behind its own lock/reconcile
// path, so concurrent client connections are safe: the server does not add its
// own dispatch goroutine, it just forwards each request to the engine, which
// funnels it through the single reconcile path shared with sampling and resets.
type Server struct {
	be            Backend
	iface         string
	kernelOK      bool
	kernelVersion string
	logf          func(string, ...any)

	ln   net.Listener
	sock string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// NewServer builds a Server. iface/kernelOK/kernelVersion are static host facts
// reported by GetHealth (the daemon owns the interface and passed the preflight
// before starting).
func NewServer(be Backend, iface string, kernelOK bool, kernelVersion string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{be: be, iface: iface, kernelOK: kernelOK, kernelVersion: kernelVersion, logf: logf, conns: map[net.Conn]struct{}{}}
}

// DaemonAlive reports whether a live daemon is already listening on the socket
// at path. It dials with a short timeout: a successful connect means another
// instance owns the socket; a refused/timed-out connect means the file is a dead
// leftover (or absent). Used both to guard Listen against double-daemon startup
// and to guard --teardown against clobbering a running daemon's ruleset.
func DaemonAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Listen binds the Unix socket at path. If a live daemon already answers on it,
// Listen refuses (returning an error) rather than removing the socket and
// stealing it — two engines rebuilding the same nft table would corrupt each
// other's state. A socket file with no live daemon behind it is a stale leftover
// from an unclean shutdown and is removed before binding.
func (s *Server) Listen(path string) error {
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if DaemonAlive(path) {
			return fmt.Errorf("another nfuse daemon is already listening on %s; stop it first", path)
		}
		// Dead socket from an unclean shutdown; safe to reclaim.
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	s.ln = ln
	s.sock = path
	return nil
}

// Serve accepts connections until Close is called. It blocks; run it in a
// goroutine if you need to do other work.
func (s *Server) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// Accept fails once the listener is closed by Close(); that is the
			// normal shutdown path, not an error to report.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.handle(conn)
	}
}

// Close stops accepting, drops open connections and removes the socket file.
func (s *Server) Close() error {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()
	if s.sock != "" {
		_ = os.Remove(s.sock)
	}
	return nil
}

func (s *Server) handle(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	sc := bufio.NewScanner(conn)
	// Allow large state payloads on the wire (default token cap is 64 KiB).
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(Response{OK: false, Error: fmt.Sprintf("bad request: %v", err)})
			continue
		}
		resp := s.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

// dispatch executes one request against the backend and builds its response.
func (s *Server) dispatch(req Request) Response {
	switch req.Method {
	case MethodGetState:
		accts, lastErr := s.be.View()
		return ok(StateResult{Accounts: accts, LastErr: lastErr})

	case MethodGetHealth:
		started, lastPersist := s.be.Stats()
		var lastUnix int64
		if !lastPersist.IsZero() {
			lastUnix = lastPersist.Unix()
		}
		return ok(HealthResult{
			Alive:           true,
			Iface:           s.iface,
			KernelOK:        s.kernelOK,
			KernelVersion:   s.kernelVersion,
			UptimeSeconds:   time.Since(started).Seconds(),
			LastPersistUnix: lastUnix,
		})

	case MethodAddAccount:
		var p AddAccountParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		id, err := s.be.AddAccount(p.Name, model.Tier(p.Tier), p.LimitGiB, p.AnchorDay)
		if err != nil {
			return fail(err)
		}
		return ok(AddAccountResult{ID: id})

	case MethodDeleteAcct:
		var p DeleteAccountParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.DeleteAccount(p.ID, p.Cascade))

	case MethodSetTier:
		var p SetTierParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.SetTier(p.ID, model.Tier(p.Tier), p.LimitGiB, p.AnchorDay))

	case MethodAddPort:
		var p AddPortParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.AddPort(p.AccountID, p.Port, p.End))

	case MethodEditPort:
		var p EditPortParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.EditPort(p.PortID, p.Start, p.End))

	case MethodDeletePort:
		var p DeletePortParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.DeletePort(p.PortID))

	case MethodMovePort:
		var p MovePortParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.MovePort(p.PortID, p.NewAccountID))

	case MethodResetAccount:
		var p ResetAccountParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.ResetAccount(p.ID))

	case MethodSetUsage:
		var p SetUsageParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		return okErr(s.be.SetUsage(p.ID, p.UsedBytes))

	case MethodForcePersist:
		return okErr(s.be.ForcePersist())

	case MethodRegenToken:
		var p RegenTokenParams
		if err := unmarshal(req.Params, &p); err != nil {
			return fail(err)
		}
		token, err := s.be.RegenerateToken(p.ID)
		if err != nil {
			return fail(err)
		}
		return ok(TokenResult{Token: token})

	case MethodGetMasterToken:
		return ok(TokenResult{Token: s.be.MasterToken()})

	case MethodRegenMasterToken:
		token, err := s.be.RegenerateMasterToken()
		if err != nil {
			return fail(err)
		}
		return ok(TokenResult{Token: token})

	default:
		return fail(fmt.Errorf("unknown method %q", req.Method))
	}
}

func unmarshal(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("missing params")
	}
	return json.Unmarshal(raw, v)
}

// ok builds a success response carrying a JSON result.
func ok(result any) Response {
	b, err := json.Marshal(result)
	if err != nil {
		return Response{OK: false, Error: fmt.Sprintf("marshal result: %v", err)}
	}
	return Response{OK: true, Result: b}
}

// okErr builds a bare ok/err response for a mutation.
func okErr(err error) Response {
	if err != nil {
		return fail(err)
	}
	return Response{OK: true}
}

func fail(err error) Response { return Response{OK: false, Error: err.Error()} }
