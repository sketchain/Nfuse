# Nfuse

Nfuse meters **per-port, bidirectional** traffic on a NIC using nftables
**netdev** hooks, persists usage to **SQLite**, and enforces per-account quotas
with an **in-kernel circuit breaker** (nftables named `quota` → per-packet
`drop`). A **TUI** manages accounts, ports, tiers and resets.

The metering fast path and the breaker are entirely in the kernel; user space
only samples, persists, resets and reconciles — so if the Go process dies, the
breaker still holds.

## Server / client roles

The same binary runs in one of two roles:

- **`nfuse --rpc`** — the **server daemon**. It runs the engine and listens on a
  Unix domain socket. It is the **only** process that touches nftables and
  SQLite (sampling, persistence, monthly resets, and every mutation go through
  its single reconcile path), so there is no write contention. This is the role
  systemd keeps running.
- **`nfuse`** (default) — the **client / TUI**. It only connects to the socket,
  renders the UI, and sends each user action as an RPC. It touches neither the
  kernel nor the DB. If the daemon is not reachable it exits with an error
  telling you to start the service — there is **no embedded-engine fallback**.

The transport is newline-delimited JSON over the socket. Mutating RPCs return
only `ok`/`err`; after a success the client re-reads full state via `GetState`.
All mutations are serialized behind the engine's reconcile lock, shared with the
sampling and reset loops, so the ruleset is never half-updated.

**Single instance.** On startup the daemon probes the socket: if a live daemon
already answers, it **refuses to start** rather than deleting the socket and
stealing it (two engines rebuilding the same nft table would corrupt each
other). Only a stale socket left by an unclean shutdown is reclaimed.

**Client reconnect.** If the daemon is restarted under an open TUI (e.g. systemd
`Restart=on-failure`), a call that hits a connection-level error redials the
socket once and replays the request, so the client recovers instead of dying;
if the daemon is still down the status bar shows the error and the next refresh
retries.

### Cold start vs hot restart

On startup the daemon probes whether its nftables table already exists to decide
the authoritative source of usage:

- **table absent = cold start** (the machine rebooted, kernel state is empty) →
  **SQLite is authoritative**: build the table seeding counters/quotas from the DB.
- **table present = hot restart** (the process was relaunched, e.g. by
  `Restart=on-failure`, but the box stayed up) → **the kernel is authoritative**:
  sample the live values, fold them into SQLite, and rebuild seeding from those,
  so a stale DB never overwrites usage the kernel is still tracking.

## How it works

### Metering (kernel, netdev @ NIC)

Two device-bound chains are attached to the managed interface (default `ens5`):

- **ingress** hook — matches **destination port** `P` (external → local port `P`)
- **egress** hook — matches **source port** `P` (local port `P` → external)

Each managed port gets **one counter per direction** (`in` = ingress/dport,
`out` = egress/sport), so the TUI can show a per-port, per-direction breakdown.
The egress hook requires **Linux ≥ 5.16**; Nfuse checks this at startup.

### Circuit breaking (kernel, named quota)

All ports of one account reference the **same named `quota` object**, so the
kernel sums that account's in + out bytes with **equal weight** into a single
budget. Each rule is laid out as:

```
<match> quota name "acct<id>" drop      # breaker: evaluated first
<match> counter name "p<portID>_<dir>"  # detail: only reached if not dropped
```

While under budget the quota statement evaluates false, the `drop` is skipped,
and the counter rule runs. Once the budget is exceeded the quota yields `drop`,
which terminates rule evaluation — so **the counter sits after the drop and
stops advancing the instant the breaker trips**. Drops are per-packet with no
graceful handling: new and established connections are treated identically.
Unlimited accounts install **no quota rule** and never break.

This is verified empirically: with a 50 KB quota, a client attempting to send
2.6 MB over loopback times out once tripped, the quota caps at `used 50000`, and
the post-drop counter freezes below the limit.

### Account tiers

| Tier | Meaning              | Kernel quota | User-space reset      |
|------|----------------------|--------------|-----------------------|
| `a`  | X GiB per month      | `over X`     | monthly (billing day) |
| `b`  | X GiB one-shot       | `over X`     | never (permanent)     |
| `c`  | unlimited            | none         | n/a                   |

Tiers **a** and **b** are byte-for-byte identical in the kernel ("drop once over
X"). The *only* difference is whether user space performs a periodic reset. The
billing anchor day must be **1–28** (values outside that range are rejected, not
clamped, so a mistyped day never silently changes the cycle).

**Switching tiers does not clear usage — by design.** Moving an account from a/b
to **c** only *pauses* metering (the quota object is removed); moving it back
revives the historical `used_bytes` and re-seeds it into the kernel quota. This
is intentional, not a bug: tier c is "billing paused", not "start over". To
actually zero usage, reset the account (`r`).

### Persistence & reboot backfill (SQLite, WAL)

Kernel quota/counter values are **lost on reboot**. Nfuse periodically snapshots
each account's quota usage and every counter to SQLite (WAL mode). On startup it
reloads those values and **seeds** the freshly created nft objects — the quota
via `used <bytes>` and counters via `packets/bytes` — so tier a/b enforcement
stays accurate across restarts. This is validated by a test that persists 500
MiB, "restarts", and confirms the rebuilt quota reports the same `used`.

Writes run on their own goroutine/ticker reading the latest in-memory sample, so
**SQLite writes never block sampling or the breaker**.

### Object lifecycle

Every mutation (add/delete account or port, tier change) is applied by **full
atomic reconciliation**: take a **fresh** live sample → fold it into SQLite →
rebuild the whole table from a single `nft -f` script (`add table; delete table;
add table`). This creates/reclaims counters, quotas and rules correctly without
hand-tracking rule handles, and can't leave the ruleset half-updated. The sample
is taken *at reconcile time* (not reused from the periodic sampler), so the
traffic metered between the last periodic sample and the rebuild is not lost; if
the live table can't be sampled the mutation is refused rather than rebuilt from
stale accounting.

A monotonic generation counter guards against a slow periodic sample racing a
usage-lowering mutation (reset, monthly reset, set-usage): a sample taken before
such a mutation completes is discarded instead of writing the pre-change value
back over it.

## Why `nft` instead of a netlink library

Nfuse shells out to `nft(8)` rather than using `google/nftables`:

- The whole ruleset is applied from one `nft -f` script, which nft runs as a
  single **atomic transaction** — race-free full reconciliation for free.
- `nft -j list` returns **structured JSON**, making counter/quota sampling a
  stable parse instead of manual netlink attribute decoding.
- Tiny dependency surface, and it matches how an operator inspects the ruleset.

The cost (a process per apply/sample) is irrelevant: applies happen only on
mutation and sampling runs at human timescales.

## Layout

```
main.go                 flags, role split (server daemon vs TUI client), signals
internal/model          domain types + invariants (account→port→counter tree)
internal/store          SQLite (WAL) load/save + reboot backfill
internal/nft            nftables manager: script generation + JSON sampling
internal/engine         sampling loop, non-blocking persistence, resets, reconcile
internal/rpc            Unix-socket JSON RPC: server (engine) + client (TUI)
internal/system         kernel-version preflight (netdev egress ≥ 5.16)
internal/tui            tview UI (drives a local engine or the RPC client)
```

## Usage

```sh
go build -o nfuse .

# Server daemon (owns nft + SQLite, no TUI) — the systemd-managed role:
sudo ./nfuse --rpc --iface ens5 --db /var/lib/nfuse/nfuse.db --socket /run/nfuse.sock

# Client TUI (connects to the daemon's socket; errors out if it can't):
sudo ./nfuse --socket /run/nfuse.sock

sudo ./nfuse --teardown                                  # remove the ruleset
```

`--teardown` refuses to run while a daemon is live on the socket (it would fight
the running engine, which rebuilds the table on its next mutation). Stop the
service first (`systemctl stop nfuse`, or kill the `--rpc` process).

The daemon requires root (nftables) and Linux ≥ 5.16. Flags: `--rpc`,
`--socket`, `--iface`, `--table`, `--db`, `--sample-interval`,
`--persist-interval`, `--ui-refresh`, `--teardown`, `--skip-kernel-check`.

### TUI keys

`a` add account · `d` delete account · `p` add port · `x` delete port ·
`m` move port · `t` change tier · `r` reset quota · `u` set usage · `q` quit

Deleting an account that still owns ports removes the account **and all of its
ports** in one atomic step; the confirmation prompt names the port count first.
A portless account is deleted directly.

The status bar shows the sampling clock (or the last error in red) plus a daemon
line from `GetHealth`: managed **iface**, **uptime**, and **last persist** time
(`never` until the first snapshot).

## Testing

```sh
go test ./...        # unit tests always run; nft integration tests need root
```

Integration tests (`internal/engine`) drive real nftables on `lo` and are
skipped automatically when `nft` is unavailable.
