package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// reconnectBackoff is the pause before a single reconnect attempt when a call
// hits a connection-level error (the daemon was restarted under us).
const reconnectBackoff = 500 * time.Millisecond

// errServerClosed marks a round-trip that failed because the server hung up
// mid-request; it is treated as a connection-level error worth reconnecting on.
var errServerClosed = errors.New("server closed connection")

// Client is the TUI-side handle to a running Nfuse daemon. Its method set
// matches what the TUI needs (the tui.Controller interface), so the same UI code
// drives either a local engine or a remote daemon.
//
// A single connection carries all traffic; a mutex serializes request/response
// round-trips so the refresh goroutine and key handlers cannot interleave on the
// wire. If the daemon is restarted (e.g. by systemd), a call that hits a
// connection-level error transparently redials the socket once and replays the
// request, so an open TUI recovers instead of dying red-screened.
type Client struct {
	mu   sync.Mutex
	path string
	conn net.Conn
	enc  *json.Encoder
	sc   *bufio.Scanner
}

// Dial connects to the daemon's Unix socket. A failure here is terminal for the
// client: the caller should surface it and exit (there is no embedded-engine
// fallback — only the daemon may touch nft/SQLite).
func Dial(path string) (*Client, error) {
	c := &Client{path: path}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

// connect dials the socket and installs a fresh encoder/scanner. Callers hold
// c.mu (or, for Dial, own the not-yet-shared Client).
func (c *Client) connect() error {
	conn, err := net.DialTimeout("unix", c.path, 3*time.Second)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.sc = sc
	return nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// call sends one request and decodes the response, retrying once over a fresh
// connection if the first attempt fails at the connection level. Params may be
// nil.
func (c *Client) call(method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.roundTrip(method, params, result)
	if err == nil || !isConnErr(err) {
		return err
	}
	// The daemon likely restarted under us: pause briefly, redial once and
	// replay this request. If reconnecting fails, surface the original error so
	// the TUI shows red and the next refresh tick tries again.
	time.Sleep(reconnectBackoff)
	if c.conn != nil {
		_ = c.conn.Close()
	}
	if rerr := c.connect(); rerr != nil {
		return err
	}
	return c.roundTrip(method, params, result)
}

// roundTrip performs a single request/response exchange on the current
// connection. Connection-level failures are wrapped so isConnErr can spot them.
func (c *Client) roundTrip(method string, params any, result any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		raw = b
	}
	if err := c.enc.Encode(Request{Method: method, Params: raw}); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return fmt.Errorf("read %s reply: %w", method, err)
		}
		return fmt.Errorf("%s: %w", method, errServerClosed)
	}
	var resp Response
	if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
		return fmt.Errorf("decode %s reply: %w", method, err)
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

// isConnErr reports whether err is a connection-level failure worth reconnecting
// on (as opposed to an application-level error the daemon returned).
func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errServerClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "use of closed network connection")
}

// View fetches the full state via GetState. On error it returns an empty view
// and surfaces the error text in the status string, matching engine.View's
// signature so the TUI can render either transparently.
func (c *Client) View() ([]engine.AccountView, string) {
	var res StateResult
	if err := c.call(MethodGetState, nil, &res); err != nil {
		return nil, err.Error()
	}
	return res.Accounts, res.LastErr
}

// Health fetches daemon health metadata.
func (c *Client) Health() (HealthResult, error) {
	var res HealthResult
	err := c.call(MethodGetHealth, nil, &res)
	return res, err
}

func (c *Client) AddAccount(name string, tier model.Tier, limitGiB float64, anchorDay int) (int64, error) {
	var res AddAccountResult
	err := c.call(MethodAddAccount, AddAccountParams{
		Name: name, Tier: string(tier), LimitGiB: limitGiB, AnchorDay: anchorDay,
	}, &res)
	return res.ID, err
}

func (c *Client) DeleteAccount(id int64, cascade bool) error {
	return c.call(MethodDeleteAcct, DeleteAccountParams{ID: id, Cascade: cascade}, nil)
}

func (c *Client) SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error {
	return c.call(MethodSetTier, SetTierParams{
		ID: id, Tier: string(tier), LimitGiB: limitGiB, AnchorDay: anchorDay,
	}, nil)
}

func (c *Client) AddPort(accountID int64, start, end uint16) error {
	return c.call(MethodAddPort, AddPortParams{AccountID: accountID, Port: start, End: end}, nil)
}

func (c *Client) EditPort(portID int64, start, end uint16) error {
	return c.call(MethodEditPort, EditPortParams{PortID: portID, Start: start, End: end}, nil)
}

func (c *Client) DeletePort(portID int64) error {
	return c.call(MethodDeletePort, DeletePortParams{PortID: portID}, nil)
}

func (c *Client) MovePort(portID, newAccountID int64) error {
	return c.call(MethodMovePort, MovePortParams{PortID: portID, NewAccountID: newAccountID}, nil)
}

func (c *Client) ResetAccount(id int64) error {
	return c.call(MethodResetAccount, ResetAccountParams{ID: id}, nil)
}

func (c *Client) SetUsage(id int64, usedBytes uint64) error {
	return c.call(MethodSetUsage, SetUsageParams{ID: id, UsedBytes: usedBytes}, nil)
}

func (c *Client) ForcePersist() error {
	return c.call(MethodForcePersist, nil, nil)
}

func (c *Client) RegenerateToken(id int64) (string, error) {
	var res TokenResult
	err := c.call(MethodRegenToken, RegenTokenParams{ID: id}, &res)
	return res.Token, err
}

func (c *Client) MasterToken() (string, error) {
	var res TokenResult
	err := c.call(MethodGetMasterToken, nil, &res)
	return res.Token, err
}

func (c *Client) RegenerateMasterToken() (string, error) {
	var res TokenResult
	err := c.call(MethodRegenMasterToken, nil, &res)
	return res.Token, err
}
