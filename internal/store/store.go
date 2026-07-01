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
	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT NOT NULL UNIQUE,
    tier               TEXT NOT NULL,
    limit_gib          REAL NOT NULL DEFAULT 0,
    billing_anchor_day INTEGER NOT NULL DEFAULT 1,
    used_bytes         INTEGER NOT NULL DEFAULT 0,
    last_reset_unix    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ports (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    port       INTEGER NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS counters (
    port_id INTEGER NOT NULL REFERENCES ports(id) ON DELETE CASCADE,
    dir     TEXT NOT NULL,
    packets INTEGER NOT NULL DEFAULT 0,
    bytes   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (port_id, dir)
);
`
	_, err := s.db.Exec(schema)
	return err
}

// Load reads the full desired-state snapshot from the database.
func (s *Store) Load() (model.Snapshot, error) {
	snap := model.Snapshot{Counters: map[model.CounterKey]model.Counter{}}

	rows, err := s.db.Query(`SELECT id, name, tier, limit_gib, billing_anchor_day, used_bytes, last_reset_unix FROM accounts ORDER BY id`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var a model.Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Tier, &a.LimitGiB, &a.BillingAnchorDay, &a.UsedBytes, &a.LastResetUnix); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Accounts = append(snap.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}

	prows, err := s.db.Query(`SELECT id, account_id, port FROM ports ORDER BY port`)
	if err != nil {
		return snap, err
	}
	for prows.Next() {
		var p model.Port
		var port int
		if err := prows.Scan(&p.ID, &p.AccountID, &port); err != nil {
			prows.Close()
			return snap, err
		}
		p.Port = uint16(port)
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

// CreateAccount inserts a new account and returns its id.
func (s *Store) CreateAccount(a model.Account) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO accounts (name, tier, limit_gib, billing_anchor_day, used_bytes, last_reset_unix)
		 VALUES (?, ?, ?, ?, 0, ?)`,
		a.Name, string(a.Tier), a.LimitGiB, a.BillingAnchorDay, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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

	res, err := tx.Exec(`INSERT INTO ports (account_id, port) VALUES (?, ?)`, p.AccountID, int(p.Port))
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
