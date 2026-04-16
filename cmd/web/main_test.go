package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// validWebEnv mirrors the env map used in internal/config tests but lives
// here as a small local helper so cmd/web tests stay self-contained.
func validWebEnv() map[string]string {
	return map[string]string{
		"BASE_URL":              "https://send.example.com",
		"BRAND_URL":             "https://brand.example.com",
		"DB_PATH":               "/var/lib/yacht/meta.db",
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
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestRun_HappyPath(t *testing.T) {
	applyEnv(t, validWebEnv())
	if err := run(context.Background(), discardLogger()); err != nil {
		t.Fatalf("run returned unexpected error: %v", err)
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
