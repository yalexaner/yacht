package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// validBotEnv mirrors the env map used in internal/config tests but lives
// here as a small local helper so cmd/bot tests stay self-contained.
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
	applyEnv(t, validBotEnv())
	if err := run(context.Background(), discardLogger()); err != nil {
		t.Fatalf("run returned unexpected error: %v", err)
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
