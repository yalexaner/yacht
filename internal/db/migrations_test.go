package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// openTestDB wires a fresh on-disk SQLite DB in t.TempDir() through the
// production New constructor so the WAL + fk pragmas are in effect for
// every migration test — the real code paths, not :memory:.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	handle, err := New(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Errorf("handle.Close: %v", err)
		}
	})
	return handle
}

// countSchemaMigrations returns the number of rows in schema_migrations.
// Used by idempotency checks and as a sanity probe after rollback tests.
func countSchemaMigrations(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

// embeddedMigrationCount asks the real embedded FS (the one shipped in the
// package) how many .sql files it contains, so the "applies all" assertion
// doesn't hard-code a number that will drift as Task 3 lands.
func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	sub, err := fs.Sub(migrationsFS, migrationsDir)
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	names, err := listMigrations(sub)
	if err != nil {
		t.Fatalf("listMigrations: %v", err)
	}
	return len(names)
}

// TestMigrate_AppliesAll checks the happy path end-to-end: run Migrate once
// against a fresh DB and every embedded file ends up recorded in
// schema_migrations with a non-zero applied_at. We count against the real
// embed rather than a fixed integer so new migrations landing in future
// tasks don't force a test edit here.
func TestMigrate_AppliesAll(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want := embeddedMigrationCount(t)
	got := countSchemaMigrations(t, db)
	if got != want {
		t.Fatalf("schema_migrations rows = %d, want %d (one per embedded file)", got, want)
	}

	// applied_at must be non-zero for every recorded file, otherwise the
	// runner is silently inserting bogus timestamps.
	rows, err := db.QueryContext(ctx, `SELECT filename, applied_at FROM schema_migrations`)
	if err != nil {
		t.Fatalf("select schema_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var appliedAt int64
		if err := rows.Scan(&name, &appliedAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if appliedAt == 0 {
			t.Errorf("applied_at for %q is zero", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
}

// TestMigrate_Idempotent locks in the "safe to re-run" invariant that both
// binaries rely on at startup: a second Migrate call neither errors nor
// appends additional schema_migrations rows.
func TestMigrate_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (first): %v", err)
	}
	first := countSchemaMigrations(t, db)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (second): %v", err)
	}
	second := countSchemaMigrations(t, db)

	if first != second {
		t.Errorf("row count changed across re-run: first=%d, second=%d", first, second)
	}
}

// TestMigrateFS_EmptyIsNoOp covers the edge case called out in Technical
// Details: if the embedded FS is empty, Migrate should still bootstrap
// schema_migrations (so later runs can proceed) but record zero rows.
func TestMigrateFS_EmptyIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := migrateFS(ctx, db, fstest.MapFS{}); err != nil {
		t.Fatalf("migrateFS (empty): %v", err)
	}

	if n := countSchemaMigrations(t, db); n != 0 {
		t.Errorf("schema_migrations rows = %d on empty FS, want 0", n)
	}
}

// TestMigrateFS_BadMigrationRollsBack injects an fstest.MapFS with a
// syntactically-invalid statement alongside a CREATE TABLE that would
// succeed on its own. After Migrate returns the error, the SQL side of
// the bad file must not have landed (the `good_table` CREATE is part of
// the same transaction) and schema_migrations must not have a row for
// the bad filename.
func TestMigrateFS_BadMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	const badName = "002_bad.sql"
	fsys := fstest.MapFS{
		badName: &fstest.MapFile{
			// the first statement is legal; the second is a parse error.
			// if the runner's transaction boundary is working, both are
			// rolled back together.
			Data: []byte(`CREATE TABLE good_table (id INTEGER PRIMARY KEY);
CREATE TABLE invalid FROM;`),
		},
	}

	err := migrateFS(ctx, db, fsys)
	if err == nil {
		t.Fatalf("migrateFS returned nil error, want error for bad sql")
	}
	if !strings.Contains(err.Error(), badName) {
		t.Errorf("error should mention %q, got %q", badName, err.Error())
	}

	// schema_migrations must exist (bootstrap ran) but have no row for the
	// failed file.
	var recorded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE filename = ?`, badName).
		Scan(&recorded); err != nil {
		t.Fatalf("count bad migration row: %v", err)
	}
	if recorded != 0 {
		t.Errorf("schema_migrations has %d rows for %q, want 0 (rollback)", recorded, badName)
	}

	// good_table must not exist — proves the transaction rolled back the
	// CREATE that preceded the bad statement.
	var name string
	err = db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='good_table'`).
		Scan(&name)
	if err == nil {
		t.Errorf("good_table exists after rollback; rollback did not undo earlier statement")
	}
}

// TestListMigrations_SortsLexicographic locks in the ordering guarantee
// the runner depends on: no matter what order fs.ReadDir returns entries,
// callers get a lexicographically-sorted slice. Mixing the input order in
// the MapFS literal is the whole point of this test — map iteration is
// unordered in Go, so if listMigrations didn't sort we'd see failures.
func TestListMigrations_SortsLexicographic(t *testing.T) {
	fsys := fstest.MapFS{
		"003_c.sql": &fstest.MapFile{Data: []byte("-- c")},
		"001_a.sql": &fstest.MapFile{Data: []byte("-- a")},
		"002_b.sql": &fstest.MapFile{Data: []byte("-- b")},
	}

	got, err := listMigrations(fsys)
	if err != nil {
		t.Fatalf("listMigrations: %v", err)
	}

	want := []string{"001_a.sql", "002_b.sql", "003_c.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listMigrations = %v, want %v", got, want)
	}
}

// TestListMigrations_FiltersNonSQL documents the exclusion rules: non-sql
// files and sub-directories do not appear in the returned slice. This lets
// operators drop a README.md or similar next to the migrations without
// the runner attempting to execute it.
func TestListMigrations_FiltersNonSQL(t *testing.T) {
	fsys := fstest.MapFS{
		"001_real.sql": &fstest.MapFile{Data: []byte("-- real")},
		"README.md":    &fstest.MapFile{Data: []byte("# notes")},
		"notes.txt":    &fstest.MapFile{Data: []byte("hi")},
		"sub/002_nested.sql": &fstest.MapFile{
			Data: []byte("-- should be ignored, not at root"),
		},
	}

	got, err := listMigrations(fsys)
	if err != nil {
		t.Fatalf("listMigrations: %v", err)
	}

	want := []string{"001_real.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listMigrations = %v, want %v", got, want)
	}
}
