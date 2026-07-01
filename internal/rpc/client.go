package rpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// Client is the TUI-side handle to a running Nfuse daemon. Its method set
// matches what the TUI needs (the tui.Controller interface), so the same UI code
// drives either a local engine or a remote daemon.
//
// A single connection carries all traffic; a mutex serializes request/response
// round-trips so the refresh goroutine and key handlers cannot interleave on the
// wire.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	enc  *json.Encoder
	sc   *bufio.Scanner
}

// Dial connects to the daemon's Unix socket. A failure here is terminal for the
// client: the caller should surface it and exit (there is no embedded-engine
// fallback — only the daemon may touch nft/SQLite).
func Dial(path string) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &Client{conn: conn, enc: json.NewEncoder(conn), sc: sc}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// call sends one request and decodes the response. Params may be nil.
func (c *Client) call(method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

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
		return fmt.Errorf("%s: server closed connection", method)
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

func (c *Client) DeleteAccount(id int64) error {
	return c.call(MethodDeleteAcct, DeleteAccountParams{ID: id}, nil)
}

func (c *Client) SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error {
	return c.call(MethodSetTier, SetTierParams{
		ID: id, Tier: string(tier), LimitGiB: limitGiB, AnchorDay: anchorDay,
	}, nil)
}

func (c *Client) AddPort(accountID int64, port uint16) error {
	return c.call(MethodAddPort, AddPortParams{AccountID: accountID, Port: port}, nil)
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
