// Package db owns the SQLite persistence layer: connection open, pragma
// application, and schema migrations. Both binaries (cmd/web and cmd/bot)
// call into this package on startup before they service any traffic.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	// registers the "sqlite" driver on import. modernc.org/sqlite is a pure-Go
	// translation of SQLite that cross-compiles without CGO — required per
	// SPEC § Tech Stack.
	_ "modernc.org/sqlite"
)

// dsn builds the sql.Open DSN string for the given on-disk database path.
//
// SPEC § Configuration prescribes DSN params `_journal=WAL&_timeout=5000&_fk=true`,
// which is the mattn/go-sqlite3 convention. modernc.org/sqlite uses a different,
// more general syntax: repeated `_pragma=<pragma>(<value>)` query parameters
// (see the driver's applyQueryParams in sqlite.go). We translate the SPEC's
// three knobs into the modernc form so the final pragma state matches intent:
//
//   - _journal=WAL      -> _pragma=journal_mode(WAL)
//   - _timeout=5000     -> _pragma=busy_timeout(5000)
//   - _fk=true          -> _pragma=foreign_keys(true)
//
// The driver applies `busy_timeout` first and the rest in lexicographic order
// on every new pooled connection, so WAL + FK + timeout are all in effect for
// every query issued through the returned *sql.DB.
func dsn(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(true)")
	return "file:" + dbPath + "?" + q.Encode()
}

// New opens the SQLite database at dbPath, applies the WAL + foreign-keys +
// busy-timeout pragmas via the DSN, and verifies the connection is live with
// PingContext so configuration / permission errors surface at startup rather
// than on the first real query.
//
// The caller owns the returned handle and is expected to defer Close on it.
// *sql.DB is safe for concurrent use; a single handle per process is the
// intended usage (see SPEC § Architecture → Database concurrency).
func New(ctx context.Context, dbPath string) (*sql.DB, error) {
	handle, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %q: %w", dbPath, err)
	}
	if err := handle.PingContext(ctx); err != nil {
		// close the half-open handle so we don't leak it; ignore the close
		// error because the ping error is the interesting one to surface.
		_ = handle.Close()
		return nil, fmt.Errorf("ping sqlite at %q: %w", dbPath, err)
	}
	return handle, nil
}
