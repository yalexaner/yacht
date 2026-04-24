package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNew_HappyPath exercises the full production open path: a real on-disk
// SQLite file in t.TempDir(), opened with the DSN New constructs. The file
// must exist after Ping returns nil, and every pragma set by the DSN must
// be observable on the connection we get back from the pool.
func TestNew_HappyPath(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.db")

	handle, err := New(ctx, dbPath)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Errorf("handle.Close: %v", err)
		}
	})

	// open of the file was done in WAL mode — the -wal/-shm sidecar files
	// only appear after the first write, so only check the primary file.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not created at %q: %v", dbPath, err)
	}

	// Pragma verification: each pragma the DSN set must round-trip through
	// a real query. journal_mode returns "wal" (lower-case) on success,
	// foreign_keys returns 1, busy_timeout returns the configured ms value.
	t.Run("journal_mode wal", func(t *testing.T) {
		var mode string
		if err := handle.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("PRAGMA journal_mode: %v", err)
		}
		if mode != "wal" {
			t.Errorf("journal_mode = %q, want %q", mode, "wal")
		}
	})

	t.Run("foreign_keys on", func(t *testing.T) {
		var fk int
		if err := handle.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("PRAGMA foreign_keys: %v", err)
		}
		if fk != 1 {
			t.Errorf("foreign_keys = %d, want 1", fk)
		}
	})

	t.Run("busy_timeout 5000", func(t *testing.T) {
		var ms int
		if err := handle.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
			t.Fatalf("PRAGMA busy_timeout: %v", err)
		}
		if ms != 5000 {
			t.Errorf("busy_timeout = %d, want 5000", ms)
		}
	})
}

// TestNew_UnwritableParent exercises the error path: a parent directory that
// does not exist (and cannot be created because its own parent is a regular
// file) must cause New to return an error. This also documents that New does
// NOT attempt to MkdirAll — the caller is responsible for path provisioning.
func TestNew_UnwritableParent(t *testing.T) {
	ctx := context.Background()

	// Construct a path whose parent cannot possibly exist: put the DB file
	// underneath a regular file we create inside t.TempDir(). That forces
	// SQLite's open() to fail regardless of platform-specific /tmp semantics.
	barrier := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(barrier, []byte("x"), 0o600); err != nil {
		t.Fatalf("create barrier file: %v", err)
	}
	dbPath := filepath.Join(barrier, "meta.db")

	handle, err := New(ctx, dbPath)
	if err == nil {
		_ = handle.Close()
		t.Fatalf("New(%q) returned nil error, want error", dbPath)
	}
	if handle != nil {
		t.Errorf("New returned non-nil handle alongside error: %v", handle)
	}
	// the error message should reference the path so an operator can see
	// which DB failed to open in aggregated logs.
	if !strings.Contains(err.Error(), dbPath) {
		t.Errorf("error should mention path %q, got %q", dbPath, err.Error())
	}
}

// TestDSN_TranslatesSpecParams locks in the translation from the SPEC's
// documented DSN format to modernc.org/sqlite's `_pragma=...` form. If the
// SPEC's form ever becomes natively supported or this translation changes,
// this test will fail and force an intentional update rather than letting
// drift land silently.
func TestDSN_TranslatesSpecParams(t *testing.T) {
	got := dsn("/tmp/meta.db")

	// scheme + path must be preserved verbatim.
	if !strings.HasPrefix(got, "file:/tmp/meta.db?") {
		t.Errorf("dsn should start with file:/tmp/meta.db? — got %q", got)
	}

	// each SPEC knob appears as a _pragma= entry with the right content.
	wantPragmas := []string{
		"_pragma=journal_mode%28WAL%29",
		"_pragma=busy_timeout%285000%29",
		"_pragma=foreign_keys%28true%29",
	}
	for _, p := range wantPragmas {
		if !strings.Contains(got, p) {
			t.Errorf("dsn missing expected pragma %q; got %q", p, got)
		}
	}

	// _txlock=immediate is the modernc-specific knob that makes BeginTx issue
	// `BEGIN IMMEDIATE` so a deferred read-then-update transaction can't hit
	// SQLITE_BUSY_SNAPSHOT under concurrent writers (botTokenHandler relies
	// on this; see the WHY-comment on dsn).
	if !strings.Contains(got, "_txlock=immediate") {
		t.Errorf("dsn missing _txlock=immediate; got %q", got)
	}
}
