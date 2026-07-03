// Package rpc is the transport between the Nfuse client (TUI) and the server
// daemon (engine). It uses a Unix domain socket carrying newline-delimited JSON
// requests and responses — a deliberately tiny protocol matching the project's
// low-dependency, shell-out style.
//
// Roles (see the top-level command): the server (`nfuse --rpc`) is the single
// process that owns nftables and SQLite; it runs the engine and answers RPCs.
// The client (default, TUI) never touches the kernel or the DB — every action
// it takes is one of the requests below. All mutation RPCs return only ok/err;
// the client re-reads full state via GetState afterwards.
package rpc

import (
	"encoding/json"

	"github.com/sketchain/nfuse/internal/engine"
)

// Method names carried in Request.Method.
const (
	MethodGetState     = "GetState"
	MethodGetHealth    = "GetHealth"
	MethodAddAccount   = "AddAccount"
	MethodDeleteAcct   = "DeleteAccount"
	MethodSetTier      = "SetTier"
	MethodAddPort      = "AddPort"
	MethodEditPort     = "EditPort"
	MethodDeletePort   = "DeletePort"
	MethodMovePort     = "MovePort"
	MethodResetAccount = "ResetAccount"
	MethodSetUsage     = "SetUsage"
	MethodForcePersist = "ForcePersist"
)

// Request is one newline-delimited JSON call from client to server.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the server's newline-delimited JSON reply. Result is present only
// for read methods (and AddAccount, which returns the new id); mutations set OK
// with no Result and the client refreshes via GetState.
type Response struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// ── Params ───────────────────────────────────────────────────────────────────

type AddAccountParams struct {
	Name      string  `json:"name"`
	Tier      string  `json:"tier"`
	LimitGiB  float64 `json:"limit_gib"`
	AnchorDay int     `json:"anchor_day"`
}

type DeleteAccountParams struct {
	ID int64 `json:"id"`
	// Cascade, when true, deletes the account together with all of its ports.
	// It is an optional field: an older client that omits it decodes to false,
	// preserving the original "refuse if the account still owns ports" behavior.
	Cascade bool `json:"cascade,omitempty"`
}

type SetTierParams struct {
	ID        int64   `json:"id"`
	Tier      string  `json:"tier"`
	LimitGiB  float64 `json:"limit_gib"`
	AnchorDay int     `json:"anchor_day"`
}

type AddPortParams struct {
	AccountID int64 `json:"account_id"`
	// Port is the range start (kept named "port" for wire back-compat: an older
	// client sending a single port populates exactly this field).
	Port uint16 `json:"port"`
	// End is the range end. Optional: an older client omits it (End == 0), which
	// the server treats as a single port (end = start).
	End uint16 `json:"end,omitempty"`
}

type EditPortParams struct {
	PortID int64  `json:"port_id"`
	Start  uint16 `json:"start"`
	// End is optional; 0 means a single port (end = start).
	End uint16 `json:"end,omitempty"`
}

type DeletePortParams struct {
	PortID int64 `json:"port_id"`
}

type MovePortParams struct {
	PortID       int64 `json:"port_id"`
	NewAccountID int64 `json:"new_account_id"`
}

type ResetAccountParams struct {
	ID int64 `json:"id"`
}

type SetUsageParams struct {
	ID        int64  `json:"id"`
	UsedBytes uint64 `json:"used_bytes"`
}

// ── Results ──────────────────────────────────────────────────────────────────

// StateResult is the full snapshot returned by GetState: the same account/port/
// counter view the engine renders for the TUI, plus any last sampling error.
type StateResult struct {
	Accounts []engine.AccountView `json:"accounts"`
	LastErr  string               `json:"last_err,omitempty"`
}

// AddAccountResult carries the id of the newly created account.
type AddAccountResult struct {
	ID int64 `json:"id"`
}

// HealthResult answers GetHealth: liveness and operational metadata.
type HealthResult struct {
	Alive           bool    `json:"alive"`
	Iface           string  `json:"iface"`
	KernelOK        bool    `json:"kernel_ok"`
	KernelVersion   string  `json:"kernel_version"`
	UptimeSeconds   float64 `json:"uptime_seconds"`
	LastPersistUnix int64   `json:"last_persist_unix"`
}
