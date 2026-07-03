package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sketchain/nfuse/internal/model"
)

// createLegacyDB writes a pre-range database at path: the old ports schema with a
// UNIQUE `port` column and no `end` column, seeded with one account and two
// single ports. It bypasses the store so we can exercise the migration path.
func createLegacyDB(t *testing.T, path string) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE accounts (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			name               TEXT NOT NULL UNIQUE,
			tier               TEXT NOT NULL,
			limit_gib          REAL NOT NULL DEFAULT 0,
			billing_anchor_day INTEGER NOT NULL DEFAULT 1,
			used_bytes         INTEGER NOT NULL DEFAULT 0,
			last_reset_unix    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE ports (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			port       INTEGER NOT NULL UNIQUE
		)`,
		`CREATE TABLE counters (
			port_id INTEGER NOT NULL REFERENCES ports(id) ON DELETE CASCADE,
			dir     TEXT NOT NULL,
			packets INTEGER NOT NULL DEFAULT 0,
			bytes   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (port_id, dir)
		)`,
		`INSERT INTO accounts (id, name, tier, limit_gib) VALUES (1, 'legacy', 'a', 1)`,
		`INSERT INTO ports (id, account_id, port) VALUES (10, 1, 8080)`,
		`INSERT INTO ports (id, account_id, port) VALUES (20, 1, 60006)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("legacy stmt %q: %v", s, err)
		}
	}
}

// TestMigrateBackfillsEndColumn covers task 4: opening a pre-range database adds
// the `end` column and backfills end = port so every legacy single port becomes
// the closed range [port, port], and re-opening is idempotent (values unchanged).
func TestMigrateBackfillsEndColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDB(t, path)

	// First open runs the migration.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	assertBackfilled := func(when string, s *Store) {
		snap, err := s.Load()
		if err != nil {
			t.Fatalf("%s load: %v", when, err)
		}
		if len(snap.Ports) != 2 {
			t.Fatalf("%s: got %d ports, want 2", when, len(snap.Ports))
		}
		for _, p := range snap.Ports {
			if p.End != p.Start {
				t.Fatalf("%s: port id %d = %d-%d, want end == start (backfilled)", when, p.ID, p.Start, p.End)
			}
		}
		// Spot-check the specific rows survived with their ids and numbers.
		want := map[int64]uint16{10: 8080, 20: 60006}
		for _, p := range snap.Ports {
			if w, ok := want[p.ID]; !ok || p.Start != w {
				t.Fatalf("%s: unexpected port id=%d start=%d", when, p.ID, p.Start)
			}
		}
	}
	assertBackfilled("after first open", st)
	st.Close()

	// Re-open: migration must be a no-op and the data unchanged.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (idempotent migrate): %v", err)
	}
	defer st2.Close()
	assertBackfilled("after reopen", st2)

	// The migrated store must accept a real range and read it back intact.
	if _, err := st2.CreatePort(model.Port{AccountID: 1, Start: 61000, End: 61099}); err != nil {
		t.Fatalf("create range on migrated db: %v", err)
	}
	snap, err := st2.Load()
	if err != nil {
		t.Fatalf("load after range insert: %v", err)
	}
	var sawRange bool
	for _, p := range snap.Ports {
		if p.Start == 61000 && p.End == 61099 {
			sawRange = true
		}
	}
	if !sawRange {
		t.Fatalf("range 61000-61099 not stored/read back: %+v", snap.Ports)
	}
}

// TestFreshDBHasEndColumn covers the fresh-schema path: a brand-new database
// created by Open already stores ranges without any migration step.
func TestFreshDBHasEndColumn(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	defer st.Close()
	if _, err := st.CreateAccount(model.Account{Name: "a", Tier: model.TierUnlimited}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := st.CreatePort(model.Port{AccountID: 1, Start: 60000, End: 60099}); err != nil {
		t.Fatalf("create range: %v", err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(snap.Ports) != 1 || snap.Ports[0].Start != 60000 || snap.Ports[0].End != 60099 {
		t.Fatalf("fresh db range = %+v, want one 60000-60099", snap.Ports)
	}
}
