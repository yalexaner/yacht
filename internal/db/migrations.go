package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// migrationsFS holds every SQL migration file that ships with this package.
// The files follow an `NNN_description.sql` convention with a zero-padded
// three-digit prefix so that lexicographic sort matches numerical order —
// see listMigrations.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is the path prefix used by the //go:embed directive above.
// It is kept as a named constant so the directive and the fs.Sub call stay
// in sync if the layout ever moves.
const migrationsDir = "migrations"

// schemaMigrationsDDL bootstraps the tracking table that records which
// migrations have already been applied to this database. The runner calls
// it on every Migrate invocation (idempotent via IF NOT EXISTS) before it
// starts comparing the embedded files against the recorded ones.
const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	filename   TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
)`

// Migrate applies every pending migration embedded at build time into the
// given database, inside its own transaction per file, and records each
// successful application in the schema_migrations table. Calling Migrate
// twice on the same database is safe: the second call is a no-op and
// returns 0 as the applied count.
//
// The returned count is the number of migrations applied by this call
// specifically — already-applied files do not contribute. Callers typically
// log this at INFO on startup so operators can tell at a glance whether a
// deploy brought new schema changes.
//
// Migrations are sorted lexicographically by filename; the `NNN_` prefix
// convention makes that equivalent to numerical ordering up to 999 files.
// On the first error the call returns, wrapped with the offending filename,
// so operators can tell exactly which migration failed in aggregated logs.
func Migrate(ctx context.Context, db *sql.DB) (int, error) {
	sub, err := fs.Sub(migrationsFS, migrationsDir)
	if err != nil {
		// the embedded FS is built from //go:embed above, so this should
		// only happen if the embed directive and migrationsDir disagree.
		return 0, fmt.Errorf("sub migrations fs: %w", err)
	}
	return migrateFS(ctx, db, sub)
}

// migrateFS is the testable core of Migrate. It takes an fs.FS rooted at
// the migrations directory (i.e. the files are at the FS root, not under
// a "migrations/" subpath) so that tests can substitute an fstest.MapFS
// without having to mirror the real directory layout.
//
// The int return mirrors Migrate: number of files this invocation actually
// applied (i.e. excluding ones already recorded in schema_migrations).
func migrateFS(ctx context.Context, db *sql.DB, fsys fs.FS) (int, error) {
	if _, err := db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return 0, fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	names, err := listMigrations(fsys)
	if err != nil {
		return 0, fmt.Errorf("list migrations: %w", err)
	}

	var applied int
	for _, name := range names {
		already, err := migrationApplied(ctx, db, name)
		if err != nil {
			return applied, fmt.Errorf("check %s: %w", name, err)
		}
		if already {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return applied, fmt.Errorf("read %s: %w", name, err)
		}
		didApply, err := applyMigration(ctx, db, name, string(body))
		if err != nil {
			return applied, fmt.Errorf("apply %s: %w", name, err)
		}
		if didApply {
			applied++
		}
	}
	return applied, nil
}

// listMigrations returns the `.sql` filenames at the root of fsys, sorted
// lexicographically. Non-sql files and sub-directories are filtered out so
// that auxiliary files (README, .gitkeep, etc.) can live alongside
// migrations without being mistaken for them.
func listMigrations(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".sql") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// migrationApplied returns true if schema_migrations already has a row for
// the given filename. An sql.ErrNoRows on the row scan means "not applied
// yet" and is the expected signal for the first-time apply path; any other
// error is surfaced as-is so the caller can wrap it with the filename.
func migrationApplied(ctx context.Context, db *sql.DB, filename string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM schema_migrations WHERE filename = ?`, filename).
		Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// applyMigration runs a single migration's SQL inside its own transaction
// and records the application in schema_migrations atomically. On any
// error the transaction rolls back — so a partially-applied migration
// never lands in the database and schema_migrations only ever reflects
// successfully-committed files.
//
// The transaction starts with `BEGIN IMMEDIATE` (vs the stdlib default,
// which is `BEGIN DEFERRED`) so the SQLite write lock is acquired at
// transaction start rather than lazily on the first write. This serializes
// migration runners across processes: when both binaries start together
// and both see a migration as missing, the second to call BEGIN IMMEDIATE
// blocks (up to busy_timeout) until the first commits — at which point
// the recheck below sees the migration is now applied and the second
// process commits an empty tx instead of re-running the DDL and exiting
// with "table already exists". Without this, the loser of the race would
// crash the binary at startup.
//
// The bool return distinguishes "this call applied the migration" from
// "this call lost a race and observed the migration as already applied
// after the lock came in" — only the former contributes to the applied
// count surfaced by Migrate.
func applyMigration(ctx context.Context, db *sql.DB, filename, body string) (applied bool, retErr error) {
	// pin a single connection so all the statements (BEGIN IMMEDIATE, the
	// migration body, COMMIT) land on the same SQLite session. Releasing it
	// back to the pool only happens after the tx is resolved.
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire conn: %w", err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("release conn: %w", cerr)
		}
	}()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// rollback errors are swallowed — the caller already has the real
		// failure to surface, and "rolling back a failed tx failed" is not
		// actionable separately. Use a fresh context so a cancelled parent
		// doesn't prevent the cleanup from reaching SQLite.
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	}()

	// recheck inside the write lock: another process may have applied this
	// file between our outer migrationApplied() call and our acquiring the
	// lock here. If so, this is the loser of the race — commit the empty
	// tx and report not-applied so the count Migrate surfaces stays honest.
	var one int
	err = conn.QueryRowContext(ctx,
		`SELECT 1 FROM schema_migrations WHERE filename = ?`, filename).Scan(&one)
	if err == nil {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return false, fmt.Errorf("commit after race: %w", err)
		}
		committed = true
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("recheck inside lock: %w", err)
	}

	if _, err := conn.ExecContext(ctx, body); err != nil {
		return false, fmt.Errorf("exec sql: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schema_migrations (filename, applied_at) VALUES (?, strftime('%s','now'))`,
		filename); err != nil {
		return false, fmt.Errorf("record in schema_migrations: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return true, nil
}
