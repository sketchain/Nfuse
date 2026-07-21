# Nfuse

Nfuse meters **per-port, bidirectional** traffic on a NIC using nftables
**netdev** hooks, persists usage to **SQLite**, and enforces per-account quotas
with an **in-kernel circuit breaker** (nftables named `quota` → per-packet
`drop`). A **subcommand CLI** manages accounts, ports, tiers and resets — with
an interactive **TUI** available as `nfuse tui`.

The metering fast path and the breaker are entirely in the kernel; user space
only samples, persists, resets and reconciles — so if the Go process dies, the
breaker still holds.

## Installation
Use the command below to install Nfuse to your system:
`sudo bash <(curl -fsSL https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse.sh)`


## Command structure

`nfuse` is a subcommand-driven CLI. The **operational** commands are the
first-class citizens: each is a thin RPC client that drives one daemon method
over the socket. The **lifecycle** commands own the process role.

```
nfuse server   --iface X   run the engine daemon (owns nft + SQLite)
nfuse tui                  interactive terminal UI against a running daemon
nfuse teardown             remove the kernel ruleset and exit
nfuse version              print version information

nfuse list [--json]                account/port/usage listing
nfuse add <name> --tier a|b|c …     add an account
nfuse rm <account> [--cascade]      delete an account
nfuse set-tier <account> --tier …   change tier / limit
nfuse reset <account>               zero an account's usage
nfuse set-usage <account> <bytes>   set an account's usage
nfuse persist                       force a SQLite snapshot now
nfuse port add  <account> <start[-end]>
nfuse port edit <port-id> <start[-end]>
nfuse port rm   <port-id>
nfuse port move <port-id> <account>

nfuse token show   <account>        print an account's HTTP query token
nfuse token new    <account>        regenerate an account's query token
nfuse token master [--new]          print (or regenerate) the master token
```

A bare `nfuse` (no subcommand) prints usage and exits non-zero; it no longer
launches the TUI — run `nfuse tui` for that.

### Server / client roles

The same binary runs in one of two roles:

- **`nfuse server`** — the **server daemon**. It runs the engine and listens on
  a Unix domain socket. It is the **only** process that touches nftables and
  SQLite (sampling, persistence, monthly resets, and every mutation go through
  its single reconcile path), so there is no write contention. This is the role
  systemd keeps running.
- **the client commands** (`nfuse tui`, `nfuse list`, `nfuse add`, …) — they
  only connect to the socket and send each action as an RPC. They touch neither
  the kernel nor the DB. If the daemon is not reachable they exit non-zero with
  an error telling you to start the service — there is **no embedded-engine
  fallback**.

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

Two device-bound chains are attached to the managed interface (`--iface`, e.g.
`ens5`):

- **ingress** hook — matches **destination port** `P` (external → local port `P`)
- **egress** hook — matches **source port** `P` (local port `P` → external)

Each managed port gets **one counter per direction** (`in` = ingress/dport,
`out` = egress/sport), so the TUI can show a per-port, per-direction breakdown.
The egress hook requires **Linux ≥ 5.16**; Nfuse checks this at startup.

#### Port ranges

A managed entry may be a single port (`60006`) or a **contiguous range**
(`60000-60099`), entered in that `start-end` form (surrounding blanks such as
`60000 - 60099` are tolerated). A range is metered and circuit-broken **as one
whole**: it renders as nftables' native range match (`th dport 60000-60099`) and
gets **exactly one counter pair** (in/out) for the entire span — there is no
per-port breakdown within a range. Its bytes fold into the owning account's
shared quota just like any other port.

Ranges (and single ports) must **not overlap** — anywhere, across all accounts.
Adjacent ranges are fine (`60000-60099` and `60100-60199` coexist); any shared
port number is rejected. The check is enforced atomically inside the engine's
reconcile path (so two concurrent adds can't both slip an overlapping range in)
and again in `Snapshot.Validate` as the last line before every kernel apply. A
range is well-formed when `1 ≤ start ≤ end ≤ 65535`.

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
main.go                 thin shim: inject build metadata, hand off to internal/cli
internal/cli            subcommand dispatch, operational commands, --json output
internal/model          domain types + invariants (account→port→counter tree)
internal/store          SQLite (WAL) load/save + reboot backfill
internal/nft            nftables manager: script generation + JSON sampling
internal/engine         sampling loop, non-blocking persistence, resets, reconcile
internal/rpc            Unix-socket JSON RPC: server (engine) + client (consumers)
internal/httpapi        read-only HTTP query endpoint (curl <host:port>/<token>)
internal/system         kernel-version preflight (netdev egress ≥ 5.16)
internal/tui            tview UI (drives a local engine or the RPC client)
```

## Usage

```sh
go build -o nfuse .

# Server daemon (owns nft + SQLite, no TUI) — the systemd-managed role:
sudo ./nfuse server --iface ens5 --db /var/lib/nfuse/nfuse.db --socket /run/nfuse.sock

# Manage accounts and ports from the shell (each is one RPC to the daemon):
sudo ./nfuse add web --tier a --limit 100 --anchor 1   # 100 GiB/month, resets on the 1st
sudo ./nfuse port add web 60000-60099                  # meter a port range
sudo ./nfuse list                                      # human-readable listing
sudo ./nfuse list --json | jq '.[] | {name, used_bytes}'

# Interactive TUI (connects to the daemon's socket; errors out if it can't):
sudo ./nfuse tui --socket /run/nfuse.sock

sudo ./nfuse teardown                                  # remove the ruleset
```

`teardown` refuses to run while a daemon is live on the socket (it would fight
the running engine, which rebuilds the table on its next mutation). Stop the
service first (`systemctl stop nfuse`, or kill the `nfuse server` process).

The daemon requires root (nftables) and Linux ≥ 5.16. `--iface` is **required
for `nfuse server`** (there is no default): the daemon refuses to start if it is
empty or names an interface that doesn't exist on the host — pick one from
`ip -br link`. `teardown` and the client commands don't need it. Server flags:
`--socket`, `--iface`, `--table`, `--db`, `--sample-interval`,
`--persist-interval`, `--skip-kernel-check`, `--http-addr` / `--http-port`
(HTTP query endpoint; **off by default** — set `--http-port` to enable it, bound
to `--http-addr`, default `127.0.0.1`). `nfuse tui` takes `--socket` and
`--ui-refresh`. Every operational command shares `--socket`.

### Scripting with `--json`

`nfuse list --json` writes a stable JSON array to **stdout** (logs and errors go
to **stderr**, never mixed in), so it drops straight into `jq`, billing jobs or
other automation. Usage is reported as **raw bytes** (`used_bytes`), and every
port carries its numeric **id** — the handle `port edit`, `port rm` and
`port move` take. An empty account list is `[]`, not `null`. Exit code is `0` on
success and non-zero on failure so scripts can branch on it.

```json
[
  {
    "id": 1, "name": "web", "tier": "a",
    "limit_gib": 100, "limit_bytes": 107374182400, "used_bytes": 5368709120,
    "ports": [ { "id": 7, "start": 60000, "end": 60099 } ]
  }
]
```

Account-referencing commands accept a **name or a numeric id**: the name is
resolved first against the live account list, and an ambiguous name (or a
missing account) is an error rather than a guess. `port` commands take the
numeric port id shown by `nfuse list`.

### HTTP query (`curl <host:port>/<token>`)

The **server daemon** can also expose a small **read-only HTTP query endpoint** so
an operator or billing script can read usage with plain `curl` — no socket, no
client binary. It is started **only by `nfuse server`** (never by `nfuse tui` or
the operational commands) and is **off by default**: it starts only when you set
`--http-port`. The bind address is `--http-addr` (default `127.0.0.1`), so even
once enabled it stays local-only until you deliberately open it:

```sh
sudo ./nfuse server --iface ens5 --http-port 8787                     # enable, loopback only
sudo ./nfuse server --iface ens5 --http-port 8787 --http-addr 0.0.0.0 # enable on all NICs
sudo ./nfuse server --iface ens5                                      # endpoint off (default)
```

Access is by **token in the URL path**. Every account has its own token, and there
is one **master token** that returns every account at once:

```sh
TOK=$(sudo ./nfuse token show web)         # this account's token
curl http://127.0.0.1:8787/$TOK            # → just this account's usage
curl "http://127.0.0.1:8787/$TOK?format=json"   # machine-readable JSON (or ?json)

MTOK=$(sudo ./nfuse token master)          # the master token
curl http://127.0.0.1:8787/$MTOK           # → every account's usage
```

- **Tokens are 16 random mixed-case letters**, generated by the daemon and **not
  customizable**. `nfuse token new <account>` rotates an account's token (the old
  one 404s immediately); `nfuse token master --new` rotates the master token.
- **Output formats**: plain text (default) or JSON via `?format=json` (bare `?json`
  works too). The JSON is always an array — one element for an account token, one
  per account for the master token — and reports `used_bytes`/`limit_bytes` as raw
  bytes. A token is **never** echoed back in a response.
- An **unknown or empty token** is `404`; only `GET`/`HEAD` are accepted.

**Legacy databases migrate transparently.** A database created before tokens
existed has no `token` column and no master token; the first time the daemon opens
it, every account is given a fresh unique token and a master token is generated,
**without touching any existing account/port/usage data**. The step is idempotent,
so restarts never re-roll the tokens.

### Running under systemd

```ini
[Unit]
Description=Nfuse traffic-metering daemon
After=network-online.target
Wants=network-online.target

[Service]
# --iface is required; substitute your NIC (see `ip -br link`).
ExecStart=/usr/local/bin/nfuse server --iface ens5 --db /var/lib/nfuse/nfuse.db --socket /run/nfuse.sock
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

If the NIC isn't up yet the daemon exits non-zero and `Restart=on-failure`
retries until it appears — the intended behavior rather than metering the wrong
(or no) interface.

### TUI keys

`a` add account · `d` delete account · `p` add port · `e` edit port ·
`x` delete port · `m` move port · `t` change tier · `r` reset quota ·
`u` set usage · `q` quit

**Adding / editing a port** accepts either form — a single port (`60006`) or a
range (`60000-60099`). `e` edits the selected port row: renumber a single port,
shift a range's bounds, or convert between the two, pre-filled with the current
value. Because kernel counters are keyed by the port's internal id (not its
number), an edit keeps the id and so **preserves the port's accumulated
in/out counters** across the change. Sliding a range so it overlaps *its own*
old extent (e.g. `60000-60099` → `60001-60100`) is a legal move; overlapping a
*different* port is rejected.

Deleting an account that still owns ports removes the account **and all of its
ports** in one atomic step; the confirmation prompt names the port count first.
A portless account is deleted directly.

**Clickable help bar.** The shortcut strip at the bottom is a nano-style bar:
every item is **mouse-clickable** and triggers exactly the same action as its
key (same selected-row guard, same error prompts). When the terminal is too
narrow to fit the strip on one line it **wraps** onto extra rows and the bar
grows to fit; while a modal or form is open, clicks on the bar are swallowed by
the overlay just like any other click outside it.

The status bar shows the sampling clock (or the last error in red) plus a daemon
line from `GetHealth`: managed **iface**, **uptime**, and **last persist** time
(`never` until the first snapshot).

## Testing

```sh
go test ./...        # unit tests always run; nft integration tests need root
```

Integration tests (`internal/engine`) drive real nftables on `lo` and are
skipped automatically when `nft` is unavailable.
