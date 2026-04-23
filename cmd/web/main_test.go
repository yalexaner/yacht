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
// DB_PATH is deliberately omitted: tests that drive run() must set it to a
// t.TempDir()-based path so the real sqlite.Open + Migrate codepaths land
// against a writable file. Tests that exercise only config validation will
// surface DB_PATH in the aggregated LoadWeb error if unset — which is the
// intended fail-loud behaviour for forgotten overrides.
func validWebEnv() map[string]string {
	return map[string]string{
		"BASE_URL":              "https://send.example.com",
		"BRAND_URL":             "https://brand.example.com",
		"DEFAULT_LANG":          "en",
		"DEFAULT_EXPIRY_HOURS":  "24",
		"MAX_UPLOAD_BYTES":      "104857600",
		"STORAGE_BACKEND":       "r2",
		"R2_ACCOUNT_ID":         "acct-123",
		"R2_ACCESS_KEY_ID":      "AKIDEXAMPLE1234567890",
		"R2_SECRET_ACCESS_KEY":  "secret-ABCDEFGHIJKLMNOP",
		"R2_BUCKET":             "yacht-shares",
		"R2_ENDPOINT":           "https://acct.r2.cloudflarestorage.com",
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
	// set DB_PATH to a writable tempdir so run() actually exercises
	// db.New + db.Migrate against a real SQLite file. validWebEnv leaves
	// DB_PATH unset so a forgotten override fails loudly in LoadWeb.
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	env["DB_PATH"] = dbPath
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
