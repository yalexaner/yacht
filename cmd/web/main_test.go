package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yalexaner/yacht/internal/db"
)

// validWebEnv mirrors the env map used in internal/config tests but lives
// here as a small local helper so cmd/web tests stay self-contained.
//
// DB_PATH defaults to a sentinel production path; individual tests that
// actually drive run() should override it with a t.TempDir()-based path so
// the real sqlite.Open + Migrate codepaths land against a writable file.
func validWebEnv() map[string]string {
	return map[string]string{
		"BASE_URL":              "https://send.example.com",
		"BRAND_URL":             "https://brand.example.com",
		"DB_PATH":               "/var/lib/yacht/meta.db",
		"DEFAULT_LANG":          "en",
		"DEFAULT_EXPIRY_HOURS":  "24",
		"MAX_UPLOAD_BYTES":      "104857600",
		"STORAGE_BACKEND":       "local",
		"STORAGE_LOCAL_PATH":    "/var/lib/yacht/files",
		"HTTP_LISTEN":           "127.0.0.1:8080",
		"SESSION_COOKIE_NAME":   "yacht_session",
		"SESSION_LIFETIME_DAYS": "30",
		"TELEGRAM_BOT_USERNAME": "yachtshare_bot",
		"TELEGRAM_BOT_TOKEN":    "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}
}

func applyEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	// If the test intentionally omits DB_PATH, clear any inherited value
	// from the parent shell so LoadWeb sees it as missing. Without this,
	// a developer who exports DB_PATH for local runs would silently bypass
	// the fail-loud contract that validWebEnv establishes by omission.
	if _, ok := vars["DB_PATH"]; !ok {
		t.Setenv("DB_PATH", "")
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestRun_HappyPath(t *testing.T) {
	env := validWebEnv()
	// override DB_PATH to point at a writable tempdir so run() actually
	// exercises db.New + db.Migrate against a real SQLite file. The
	// production sentinel path in validWebEnv would hit a permission
	// error under `go test`.
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "meta.db")
	env["DB_PATH"] = dbPath
	// switch to the local storage backend for the happy path so the
	// factory.New step constructs a real, working backend without needing
	// any external services. STORAGE_LOCAL_PATH must be writable for the
	// MkdirAll inside local.New to succeed.
	env["STORAGE_BACKEND"] = "local"
	env["STORAGE_LOCAL_PATH"] = filepath.Join(tmp, "objects")
	applyEnv(t, env)

	if err := run(context.Background(), discardLogger()); err != nil {
		t.Fatalf("run returned unexpected error: %v", err)
	}

	// the migration runner should have created the file on disk during
	// the Migrate step. If the file is missing, either db.New or Migrate
	// silently no-oped, both of which are bugs in the startup path.
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected db file at %q after run, got stat err: %v", dbPath, err)
	}
	assertMigrated(t, dbPath)
	// local.New MkdirAll's the storage root, so the happy path must have
	// created it. If it's missing, run() returned success without actually
	// invoking the storage factory — a silent regression we want to catch.
	if _, err := os.Stat(filepath.Join(tmp, "objects")); err != nil {
		t.Errorf("expected storage root after run, got stat err: %v", err)
	}
}

// assertMigrated reopens the DB at dbPath and fails the test if
// schema_migrations has zero rows. Proves that run() called db.Migrate,
// not just db.New — os.Stat alone can't distinguish the two.
func assertMigrated(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	h, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	var n int
	if err := h.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n == 0 {
		t.Fatal("schema_migrations has 0 rows; db.Migrate did not run")
	}
}

func TestRun_MissingRequiredVar(t *testing.T) {
	env := validWebEnv()
	// blank a required shared var so LoadWeb aggregates an error. Using empty
	// string rather than delete() because t.Setenv only sets values and some
	// parent processes may already have BASE_URL exported.
	env["BASE_URL"] = ""
	applyEnv(t, env)

	err := run(context.Background(), discardLogger())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "BASE_URL") {
		t.Errorf("err should mention BASE_URL, got %q", err.Error())
	}
}

// TestRun_StorageInitFails drives the storage-factory failure path through
// run's wrapping logic: pointing STORAGE_LOCAL_PATH at a location whose
// parent is a regular file (not a directory) causes local.New's MkdirAll to
// fail. The returned error must be wrapped with the "init storage" prefix so
// operators can tell at a glance which startup step failed.
func TestRun_StorageInitFails(t *testing.T) {
	env := validWebEnv()
	tmp := t.TempDir()
	// DB_PATH must be valid so the test actually reaches the storage step
	// rather than failing earlier. Same approach as TestRun_HappyPath.
	env["DB_PATH"] = filepath.Join(tmp, "meta.db")
	env["STORAGE_BACKEND"] = "local"
	// create a regular file and point STORAGE_LOCAL_PATH underneath it; the
	// kernel refuses to create a directory whose parent is a non-directory,
	// so MkdirAll returns an error. Same barrier-file trick as the DB test.
	barrier := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(barrier, []byte("x"), 0o600); err != nil {
		t.Fatalf("create barrier file: %v", err)
	}
	env["STORAGE_LOCAL_PATH"] = filepath.Join(barrier, "objects")
	applyEnv(t, env)

	err := run(context.Background(), discardLogger())
	if err == nil {
		t.Fatal("want error for unwritable STORAGE_LOCAL_PATH, got nil")
	}
	// lock in the "init storage:" prefix specifically so a regression that
	// swaps wraps (or mentions "storage" only indirectly) fails this test.
	if !strings.Contains(err.Error(), "init storage") {
		t.Errorf("err should mention \"init storage\", got %q", err.Error())
	}
}

// TestRun_UnwritableDBPath drives the db.New failure path through run's
// wrapping logic: a DB_PATH whose parent directory doesn't exist causes
// sqlite to fail during PingContext. The returned error must be wrapped
// with the "open database" prefix so operators can tell at a glance which
// startup step failed.
func TestRun_UnwritableDBPath(t *testing.T) {
	env := validWebEnv()
	// construct a path whose parent cannot possibly exist by putting the
	// DB file underneath a regular file created inside t.TempDir(). This
	// matches the pattern used in internal/db/db_test.go and avoids the
	// flakiness of relying on a specific absolute path not existing on
	// the test runner.
	barrier := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(barrier, []byte("x"), 0o600); err != nil {
		t.Fatalf("create barrier file: %v", err)
	}
	env["DB_PATH"] = filepath.Join(barrier, "meta.db")
	applyEnv(t, env)

	err := run(context.Background(), discardLogger())
	if err == nil {
		t.Fatal("want error for unwritable DB_PATH, got nil")
	}
	// lock in the "open database:" prefix specifically so a regression
	// that swaps the wrap order (or mentions "database" only via the
	// "migrate database:" wrap) fails this test. HasPrefix — not Contains —
	// so a wrapped error like "migrate database: open database: ..." fails.
	if !strings.HasPrefix(err.Error(), "open database:") {
		t.Errorf("err should start with \"open database:\", got %q", err.Error())
	}
}
