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
// twice on the same database is safe: the second call is a no-op.
//
// Migrations are sorted lexicographically by filename; the `NNN_` prefix
// convention makes that equivalent to numerical ordering up to 999 files.
// On the first error the call returns, wrapped with the offending filename,
// so operators can tell exactly which migration failed in aggregated logs.
func Migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, migrationsDir)
	if err != nil {
		// the embedded FS is built from //go:embed above, so this should
		// only happen if the embed directive and migrationsDir disagree.
		return fmt.Errorf("sub migrations fs: %w", err)
	}
	return migrateFS(ctx, db, sub)
}

// migrateFS is the testable core of Migrate. It takes an fs.FS rooted at
// the migrations directory (i.e. the files are at the FS root, not under
// a "migrations/" subpath) so that tests can substitute an fstest.MapFS
// without having to mirror the real directory layout.
func migrateFS(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if _, err := db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	names, err := listMigrations(fsys)
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}

	for _, name := range names {
		applied, err := migrationApplied(ctx, db, name)
		if err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		if applied {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := applyMigration(ctx, db, name, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
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
func applyMigration(ctx context.Context, db *sql.DB, filename, body string) (retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		// rollback errors are swallowed — the caller already has the real
		// failure to surface, and "rolling back a failed tx failed" is not
		// actionable separately.
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (filename, applied_at) VALUES (?, strftime('%s','now'))`,
		filename); err != nil {
		return fmt.Errorf("record in schema_migrations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
