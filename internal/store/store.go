// Package store is the SQLite persistence layer for Nfuse.
//
// Why persistence is needed: nftables named quotas and counters live only in
// kernel memory, so their consumed/byte values are lost on reboot. User space
// therefore periodically snapshots each account's usage (and each counter) to
// SQLite, and on startup the values are read back and used to *seed* freshly
// created nft objects (quota `used`, counter `packets/bytes`). That makes tier
// a/b metering accurate across restarts.
//
// The database runs in WAL mode so that the periodic persist writes proceed as
// a side path without blocking readers (the sampling loop reads from the kernel,
// not from SQLite, but WAL also keeps concurrent DB access smooth).
package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/sketchain/nfuse/internal/model"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the schema.
// WAL journaling and a busy timeout are set so writes never block the caller
// for long and readers are not blocked by the persister.
func Open(path string) (*Store, error) {
	// modernc.org/sqlite accepts PRAGMAs via the connection string.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single writer keeps SQLite happy; readers still work under WAL.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	// The ports table stores a closed range [port, "end"]. A single port is the
	// degenerate case "end" == port. The `port` column is no longer UNIQUE: the
	// old "one row per port number" rule is superseded by interval non-overlap,
	// enforced in the engine's reconcile path and in Snapshot.Validate. (Fresh
	// databases omit the constraint; legacy databases may still carry it, which
	// is harmless — non-overlapping ranges always have distinct start ports.)
	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT NOT NULL UNIQUE,
    tier               TEXT NOT NULL,
    limit_gib          REAL NOT NULL DEFAULT 0,
    billing_anchor_day INTEGER NOT NULL DEFAULT 1,
    used_bytes         INTEGER NOT NULL DEFAULT 0,
    last_reset_unix    INTEGER NOT NULL DEFAULT 0,
    token              TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ports (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    port       INTEGER NOT NULL,
    "end"      INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS counters (
    port_id INTEGER NOT NULL REFERENCES ports(id) ON DELETE CASCADE,
    dir     TEXT NOT NULL,
    packets INTEGER NOT NULL DEFAULT 0,
    bytes   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (port_id, dir)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if err := s.migratePortRange(); err != nil {
		return err
	}
	return s.ensureTokens()
}

// masterTokenKey is the meta-table key under which the master query token lives.
const masterTokenKey = "master_token"

// ensureTokens brings a pre-token database up to the token schema and guarantees
// that every account has a query token and that a master token exists. It is the
// legacy-compatibility path required by the spec ("旧数据无 token 字段时要能平滑
// 迁移… 首次加载时自动生成，不能破坏现有数据"):
//
//   - Older databases created the accounts table without a `token` column; add it
//     (defaulting to the empty string) without disturbing any existing row.
//   - Every account whose token is still empty (freshly added column, or a row
//     that predates tokens) gets a freshly generated, collision-checked token.
//   - The single master token is generated if the meta table doesn't hold one yet.
//
// The whole step is idempotent: on a database that already has tokens the ALTER
// is skipped, no account has an empty token, and the master token already exists,
// so nothing is rewritten and no existing data is touched.
func (s *Store) ensureTokens() error {
	hasToken, err := s.columnExists("accounts", "token")
	if err != nil {
		return err
	}
	if !hasToken {
		if _, err := s.db.Exec(`ALTER TABLE accounts ADD COLUMN token TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}

	// Collect the tokens already in use so backfilled/master tokens stay unique.
	used := map[string]bool{}
	rows, err := s.db.Query(`SELECT token FROM accounts WHERE token != ''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		used[t] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Ensure the master token exists, keeping it distinct from account tokens.
	master, err := s.MasterToken()
	if err != nil {
		return err
	}
	if master == "" {
		master, err = uniqueToken(used)
		if err != nil {
			return err
		}
		if err := s.SetMasterToken(master); err != nil {
			return err
		}
	}
	used[master] = true

	// Backfill any account still missing a token.
	idRows, err := s.db.Query(`SELECT id FROM accounts WHERE token = ''`)
	if err != nil {
		return err
	}
	var ids []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			return err
		}
		ids = append(ids, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		t, err := uniqueToken(used)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE accounts SET token = ? WHERE id = ?`, t, id); err != nil {
			return err
		}
		used[t] = true
	}
	return nil
}

// uniqueToken generates a fresh token not already present in used. The token
// space is astronomically larger than any account set, so this loop effectively
// never repeats, but checking keeps token→account lookups unambiguous.
func uniqueToken(used map[string]bool) (string, error) {
	for {
		t, err := model.GenerateToken()
		if err != nil {
			return "", err
		}
		if !used[t] {
			return t, nil
		}
	}
}

// migratePortRange brings a pre-range database up to the [port, "end"] schema.
// Older databases created the ports table without an "end" column; add it and
// backfill "end" = port so every legacy single port becomes the closed range
// [port, port]. The whole step is idempotent: on a database that already has the
// column and backfilled rows, the ALTER is skipped and the UPDATE matches no
// rows (a valid range always has "end" ≥ port ≥ 1, so "end" == 0 unambiguously
// marks an un-backfilled row).
func (s *Store) migratePortRange() error {
	hasEnd, err := s.columnExists("ports", "end")
	if err != nil {
		return err
	}
	if !hasEnd {
		if _, err := s.db.Exec(`ALTER TABLE ports ADD COLUMN "end" INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`UPDATE ports SET "end" = port WHERE "end" = 0`)
	return err
}

// columnExists reports whether the given table has a column of the given name.
func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dfltValue        any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Load reads the full desired-state snapshot from the database.
func (s *Store) Load() (model.Snapshot, error) {
	snap := model.Snapshot{Counters: map[model.CounterKey]model.Counter{}}

	rows, err := s.db.Query(`SELECT id, name, tier, limit_gib, billing_anchor_day, used_bytes, last_reset_unix, token FROM accounts ORDER BY id`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var a model.Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Tier, &a.LimitGiB, &a.BillingAnchorDay, &a.UsedBytes, &a.LastResetUnix, &a.Token); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Accounts = append(snap.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}

	prows, err := s.db.Query(`SELECT id, account_id, port, "end" FROM ports ORDER BY port`)
	if err != nil {
		return snap, err
	}
	for prows.Next() {
		var p model.Port
		var start, end int
		if err := prows.Scan(&p.ID, &p.AccountID, &start, &end); err != nil {
			prows.Close()
			return snap, err
		}
		p.Start = uint16(start)
		p.End = uint16(end)
		snap.Ports = append(snap.Ports, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return snap, err
	}

	crows, err := s.db.Query(`SELECT port_id, dir, packets, bytes FROM counters`)
	if err != nil {
		return snap, err
	}
	for crows.Next() {
		var c model.Counter
		if err := crows.Scan(&c.PortID, &c.Dir, &c.Packets, &c.Bytes); err != nil {
			crows.Close()
			return snap, err
		}
		snap.Counters[model.CounterKey{PortID: c.PortID, Dir: c.Dir}] = c
	}
	crows.Close()
	return snap, crows.Err()
}

// CreateAccount inserts a new account and returns its id. The caller supplies the
// query token (the engine generates a unique one before calling); an empty token
// is accepted for legacy/test callers and is backfilled by ensureTokens on the
// next Open.
func (s *Store) CreateAccount(a model.Account) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO accounts (name, tier, limit_gib, billing_anchor_day, used_bytes, last_reset_unix, token)
		 VALUES (?, ?, ?, ?, 0, ?, ?)`,
		a.Name, string(a.Tier), a.LimitGiB, a.BillingAnchorDay, time.Now().Unix(), a.Token)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetToken overwrites an account's query token (used when regenerating).
func (s *Store) SetToken(id int64, token string) error {
	_, err := s.db.Exec(`UPDATE accounts SET token = ? WHERE id = ?`, token, id)
	return err
}

// MasterToken returns the master query token, or "" if none has been set yet.
func (s *Store) MasterToken() (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, masterTokenKey).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetMasterToken stores (or replaces) the master query token.
func (s *Store) SetMasterToken(token string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		masterTokenKey, token)
	return err
}

// DeleteAccount removes an account (cascading to its ports and counters).
func (s *Store) DeleteAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	return err
}

// SetTier updates an account's tier and (for monthly) billing anchor / limit.
func (s *Store) SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error {
	_, err := s.db.Exec(
		`UPDATE accounts SET tier = ?, limit_gib = ?, billing_anchor_day = ? WHERE id = ?`,
		string(tier), limitGiB, anchorDay, id)
	return err
}

// CreatePort inserts a port under an account and initializes its two counters.
func (s *Store) CreatePort(p model.Port) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO ports (account_id, port, "end") VALUES (?, ?, ?)`,
		p.AccountID, int(p.Start), int(p.End))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, dir := range []model.Direction{model.DirIn, model.DirOut} {
		if _, err := tx.Exec(`INSERT INTO counters (port_id, dir, packets, bytes) VALUES (?, ?, 0, 0)`, id, string(dir)); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// DeletePort removes a port (cascading to its counters).
func (s *Store) DeletePort(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ports WHERE id = ?`, id)
	return err
}

// MovePort reassigns a port to a different account. Counters are keyed by port
// id, so they follow the port unchanged.
func (s *Store) MovePort(portID, newAccountID int64) error {
	_, err := s.db.Exec(`UPDATE ports SET account_id = ? WHERE id = ?`, newAccountID, portID)
	return err
}

// EditPort rewrites a port's interval boundaries. The row's id is unchanged, so
// its counters (keyed by port id) follow the edit intact and historical usage is
// preserved across the reconcile rebuild.
func (s *Store) EditPort(portID int64, start, end uint16) error {
	_, err := s.db.Exec(`UPDATE ports SET port = ?, "end" = ? WHERE id = ?`,
		int(start), int(end), portID)
	return err
}

// SetUsage overwrites an account's persisted quota "used" value to an arbitrary
// target (used by the SetUsage RPC to raise or lower recorded consumption). The
// kernel quota is re-seeded to the same value by the subsequent reconcile.
func (s *Store) SetUsage(id int64, usedBytes uint64) error {
	_, err := s.db.Exec(`UPDATE accounts SET used_bytes = ? WHERE id = ?`, usedBytes, id)
	return err
}

// PersistUsage writes a batch of live kernel readings back to SQLite. It is the
// periodic "落盘" side path: account quota usage plus every counter snapshot,
// applied in a single transaction. Called off the sampling goroutine.
func (s *Store) PersistUsage(accountUsed map[int64]uint64, counters map[model.CounterKey]model.Counter) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for id, used := range accountUsed {
		if _, err := tx.Exec(`UPDATE accounts SET used_bytes = ? WHERE id = ?`, used, id); err != nil {
			return err
		}
	}
	for key, c := range counters {
		if _, err := tx.Exec(
			`UPDATE counters SET packets = ?, bytes = ? WHERE port_id = ? AND dir = ?`,
			c.Packets, c.Bytes, key.PortID, string(key.Dir)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkReset records that an account's monthly quota was reset to zero.
func (s *Store) MarkReset(id int64, when time.Time) error {
	_, err := s.db.Exec(`UPDATE accounts SET used_bytes = 0, last_reset_unix = ? WHERE id = ?`, when.Unix(), id)
	return err
}
