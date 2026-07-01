# Nfuse

Nfuse meters **per-port, bidirectional** traffic on a NIC using nftables
**netdev** hooks, persists usage to **SQLite**, and enforces per-account quotas
with an **in-kernel circuit breaker** (nftables named `quota` → per-packet
`drop`). A **TUI** manages accounts, ports, tiers and resets.

The metering fast path and the breaker are entirely in the kernel; user space
only samples, persists, resets and reconciles — so if the Go process dies, the
breaker still holds.

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
X"). The *only* difference is whether user space performs a periodic reset.

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
atomic reconciliation**: sample live values → fold them into SQLite → rebuild the
whole table from a single `nft -f` script (`add table; delete table; add table`).
This creates/reclaims counters, quotas and rules correctly without hand-tracking
rule handles, and can't leave the ruleset half-updated. Live values are folded in
first, so a rebuild loses no accounting.

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
main.go                 flags, kernel preflight, wiring, signals
internal/model          domain types + invariants (account→port→counter tree)
internal/store          SQLite (WAL) load/save + reboot backfill
internal/nft            nftables manager: script generation + JSON sampling
internal/engine         sampling loop, non-blocking persistence, resets, reconcile
internal/system         kernel-version preflight (netdev egress ≥ 5.16)
internal/tui            tview UI
```

## Usage

```sh
go build -o nfuse .

sudo ./nfuse --iface ens5 --db /var/lib/nfuse/nfuse.db   # control plane + TUI
sudo ./nfuse --headless                                  # no TUI (daemon)
sudo ./nfuse --teardown                                  # remove the ruleset
```

Requires root (nftables) and Linux ≥ 5.16. Flags: `--iface`, `--table`, `--db`,
`--sample-interval`, `--persist-interval`, `--ui-refresh`, `--headless`,
`--teardown`, `--skip-kernel-check`.

### TUI keys

`a` add account · `d` delete account · `p` add port · `x` delete port ·
`t` change tier · `r` reset quota · `q` quit

## Testing

```sh
go test ./...        # unit tests always run; nft integration tests need root
```

Integration tests (`internal/engine`) drive real nftables on `lo` and are
skipped automatically when `nft` is unavailable.
