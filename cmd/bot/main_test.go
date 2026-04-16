package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validBotEnv mirrors the env map used in internal/config tests but lives
// here as a small local helper so cmd/bot tests stay self-contained.
//
// DB_PATH defaults to a sentinel production path; individual tests that
// actually drive run() should override it with a t.TempDir()-based path so
// the real sqlite.Open + Migrate codepaths land against a writable file.
func validBotEnv() map[string]string {
	return map[string]string{
		"BASE_URL":             "https://send.example.com",
		"BRAND_URL":            "https://brand.example.com",
		"DB_PATH":              "/var/lib/yacht/meta.db",
		"DEFAULT_LANG":         "en",
		"DEFAULT_EXPIRY_HOURS": "24",
		"MAX_UPLOAD_BYTES":     "104857600",
		"STORAGE_BACKEND":      "r2",
		"R2_ACCOUNT_ID":        "acct-123",
		"R2_ACCESS_KEY_ID":     "AKIDEXAMPLE1234567890",
		"R2_SECRET_ACCESS_KEY": "secret-ABCDEFGHIJKLMNOP",
		"R2_BUCKET":            "yacht-shares",
		"R2_ENDPOINT":          "https://acct.r2.cloudflarestorage.com",
		"TELEGRAM_BOT_TOKEN":   "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"TELEGRAM_ADMIN_IDS":   "123456789,987654321",
		"WEBHOOK_URL":          "https://send.example.com/bot/webhook",
	}
}

func applyEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestRun_HappyPath(t *testing.T) {
	env := validBotEnv()
	// override DB_PATH to point at a writable tempdir so run() actually
	// exercises db.New + db.Migrate against a real SQLite file. The
	// production sentinel path in validBotEnv would hit a permission
	// error under `go test`.
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
}

func TestRun_MissingRequiredVar(t *testing.T) {
	env := validBotEnv()
	// blank a required bot var so LoadBot aggregates an error. Using empty
	// string rather than delete() because t.Setenv only sets values and some
	// parent processes may already have TELEGRAM_BOT_TOKEN exported.
	env["TELEGRAM_BOT_TOKEN"] = ""
	applyEnv(t, env)

	err := run(context.Background(), discardLogger())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Errorf("err should mention TELEGRAM_BOT_TOKEN, got %q", err.Error())
	}
}

// TestRun_UnwritableDBPath drives the db.New failure path through run's
// wrapping logic: a DB_PATH whose parent directory doesn't exist causes
// sqlite to fail during PingContext. The returned error must be wrapped
// with the "open database" prefix so operators can tell at a glance which
// startup step failed.
func TestRun_UnwritableDBPath(t *testing.T) {
	env := validBotEnv()
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
	// "migrate database:" wrap) fails this test.
	if !strings.Contains(err.Error(), "open database") {
		t.Errorf("err should mention \"open database\", got %q", err.Error())
	}
}
