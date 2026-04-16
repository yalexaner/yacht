package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// validBotEnv extends a valid Shared r2 env with every bot-only var, ready
// for individual tests to mutate.
func validBotEnv() map[string]string {
	env := validSharedR2()
	env["TELEGRAM_BOT_TOKEN"] = "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	env["TELEGRAM_ADMIN_IDS"] = "123456789,987654321"
	env["WEBHOOK_URL"] = "https://send.example.com/bot/webhook"
	return env
}

func TestLoadBot_HappyPathWebhookSet(t *testing.T) {
	setSharedEnv(t, validBotEnv())
	cfg, err := LoadBot()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.Shared == nil {
		t.Fatal("Shared should be populated")
	}
	if cfg.TelegramBotToken != "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Errorf("TelegramBotToken = %q", cfg.TelegramBotToken)
	}
	if len(cfg.TelegramAdminIDs) != 2 {
		t.Fatalf("TelegramAdminIDs len = %d, want 2", len(cfg.TelegramAdminIDs))
	}
	if cfg.TelegramAdminIDs[0] != 123456789 || cfg.TelegramAdminIDs[1] != 987654321 {
		t.Errorf("TelegramAdminIDs = %v", cfg.TelegramAdminIDs)
	}
	if cfg.WebhookURL != "https://send.example.com/bot/webhook" {
		t.Errorf("WebhookURL = %q", cfg.WebhookURL)
	}
	// a shared field must also be reachable via the embedded pointer.
	if cfg.BaseURL != "https://send.example.com" {
		t.Errorf("embedded BaseURL = %q", cfg.BaseURL)
	}
}

func TestLoadBot_HappyPathWebhookUnset(t *testing.T) {
	env := validBotEnv()
	delete(env, "WEBHOOK_URL")
	setSharedEnv(t, env)

	cfg, err := LoadBot()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.WebhookURL != "" {
		t.Errorf("WebhookURL should be empty (long-poll mode), got %q", cfg.WebhookURL)
	}
}

func TestLoadBot_MissingTelegramBotToken(t *testing.T) {
	env := validBotEnv()
	env["TELEGRAM_BOT_TOKEN"] = ""
	setSharedEnv(t, env)

	_, err := LoadBot()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Errorf("err should mention TELEGRAM_BOT_TOKEN, got %q", err.Error())
	}
}

func TestLoadBot_MissingAdminIDs(t *testing.T) {
	env := validBotEnv()
	env["TELEGRAM_ADMIN_IDS"] = ""
	setSharedEnv(t, env)

	_, err := LoadBot()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "TELEGRAM_ADMIN_IDS") {
		t.Errorf("err should mention TELEGRAM_ADMIN_IDS, got %q", err.Error())
	}
}

func TestLoadBot_NonNumericAdminID(t *testing.T) {
	env := validBotEnv()
	env["TELEGRAM_ADMIN_IDS"] = "123456789,not-a-number"
	setSharedEnv(t, env)

	_, err := LoadBot()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TELEGRAM_ADMIN_IDS") {
		t.Errorf("err should mention TELEGRAM_ADMIN_IDS, got %q", msg)
	}
	if !strings.Contains(msg, "not-a-number") {
		t.Errorf("err should echo the invalid entry, got %q", msg)
	}
}

func TestLoadBot_InvalidWebhookURL(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantSub string
	}{
		{name: "bad scheme", value: "ftp://example.com/hook", wantSub: "scheme"},
		{name: "missing scheme", value: "example.com/hook", wantSub: "scheme"},
		{name: "missing host", value: "https://", wantSub: "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validBotEnv()
			env["WEBHOOK_URL"] = tt.value
			setSharedEnv(t, env)

			_, err := LoadBot()
			if err == nil {
				t.Fatal("want error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, "WEBHOOK_URL") {
				t.Errorf("err should mention WEBHOOK_URL, got %q", msg)
			}
			if !strings.Contains(msg, tt.wantSub) {
				t.Errorf("err should mention %q, got %q", tt.wantSub, msg)
			}
		})
	}
}

func TestLoadBot_AggregatesSharedAndBotErrors(t *testing.T) {
	// blank a required shared var AND a required bot var; the joined error
	// must list both so the operator sees every problem at once.
	env := validBotEnv()
	env["BASE_URL"] = ""
	env["TELEGRAM_BOT_TOKEN"] = ""
	setSharedEnv(t, env)

	_, err := LoadBot()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BASE_URL") {
		t.Errorf("err should mention BASE_URL (shared), got %q", msg)
	}
	if !strings.Contains(msg, "TELEGRAM_BOT_TOKEN") {
		t.Errorf("err should mention TELEGRAM_BOT_TOKEN (bot), got %q", msg)
	}
}

func TestBotLogSafe_MasksTokenAndLogsAdminIDs(t *testing.T) {
	cfg := &Bot{
		Shared: &Shared{
			BaseURL:        "https://send.example.com",
			StorageBackend: StorageBackendR2,
		},
		TelegramBotToken: "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		TelegramAdminIDs: []int64{123456789, 987654321},
		WebhookURL:       "https://send.example.com/bot/webhook",
	}

	h := &capturingHandler{}
	logger := slog.New(h)
	cfg.LogSafe(logger)

	if len(h.records) < 2 {
		t.Fatalf("expected at least 2 records (shared + bot), got %d", len(h.records))
	}

	var botRec *slog.Record
	for i := range h.records {
		if h.records[i].Message == "config.bot" {
			botRec = &h.records[i]
			break
		}
	}
	if botRec == nil {
		t.Fatal("no config.bot record was emitted")
	}
	attrs := attrMap(*botRec)

	for _, v := range attrs {
		if strings.Contains(v, cfg.TelegramBotToken) {
			t.Errorf("attr %q contains full TelegramBotToken", v)
		}
	}
	// Assert the exact masked form: "****" + last 4 chars of the token. A
	// prefix-only check would falsely pass a buggy mask that returns pure
	// "****" regardless of input, so lock in the revealed suffix too.
	if got, want := attrs["telegram_bot_token"], "****WXYZ"; got != want {
		t.Errorf("telegram_bot_token = %q, want %q", got, want)
	}
	// admin IDs must be logged verbatim (they are not secrets).
	if got := attrs["telegram_admin_ids"]; !strings.Contains(got, "123456789") || !strings.Contains(got, "987654321") {
		t.Errorf("telegram_admin_ids should include both IDs, got %q", got)
	}
	// webhook_url must be reduced to scheme+host so any userinfo, query, or
	// secret path component never lands in logs. The full URL must not leak.
	if got, want := attrs["webhook_url"], "https://send.example.com/****"; got != want {
		t.Errorf("webhook_url = %q, want %q", got, want)
	}
	if strings.Contains(attrs["webhook_url"], "/bot/webhook") {
		t.Errorf("webhook_url should not contain the original path, got %q", attrs["webhook_url"])
	}
}

func TestBotLogSafe_WebhookURLStripsSecretPathAndQuery(t *testing.T) {
	// Webhook URLs commonly embed secrets in the path or query for request
	// authentication. LogSafe must drop everything past the host so those
	// secrets never surface in startup logs.
	cfg := &Bot{
		Shared:           &Shared{StorageBackend: StorageBackendR2},
		TelegramBotToken: "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		TelegramAdminIDs: []int64{123456789},
		WebhookURL:       "https://user:pass@send.example.com/bot/s3cretp4th?token=deadbeef",
	}

	h := &capturingHandler{}
	logger := slog.New(h)
	cfg.LogSafe(logger)

	var botRec *slog.Record
	for i := range h.records {
		if h.records[i].Message == "config.bot" {
			botRec = &h.records[i]
			break
		}
	}
	if botRec == nil {
		t.Fatal("no config.bot record was emitted")
	}
	attrs := attrMap(*botRec)

	got := attrs["webhook_url"]
	if got != "https://send.example.com/****" {
		t.Errorf("webhook_url = %q, want %q", got, "https://send.example.com/****")
	}
	for _, leak := range []string{"user:pass", "s3cretp4th", "deadbeef", "token="} {
		if strings.Contains(got, leak) {
			t.Errorf("webhook_url leaks %q: %q", leak, got)
		}
	}
}

func TestBotLogSafe_EmptyWebhookRendersAsNotSet(t *testing.T) {
	cfg := &Bot{
		Shared:           &Shared{StorageBackend: StorageBackendR2},
		TelegramBotToken: "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		TelegramAdminIDs: []int64{123456789},
	}

	h := &capturingHandler{}
	logger := slog.New(h)
	cfg.LogSafe(logger)

	var botRec *slog.Record
	for i := range h.records {
		if h.records[i].Message == "config.bot" {
			botRec = &h.records[i]
			break
		}
	}
	if botRec == nil {
		t.Fatal("no config.bot record was emitted")
	}
	attrs := attrMap(*botRec)

	if got := attrs["webhook_url"]; got != "(not set)" {
		t.Errorf("webhook_url for empty value = %q, want %q", got, "(not set)")
	}
}

// TestBotLogSafe_TextHandlerSmoke confirms the masking promise holds when a
// real text handler renders the record.
func TestBotLogSafe_TextHandlerSmoke(t *testing.T) {
	cfg := &Bot{
		Shared:           &Shared{StorageBackend: StorageBackendR2},
		TelegramBotToken: "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		TelegramAdminIDs: []int64{123456789},
		WebhookURL:       "https://send.example.com/bot/s3cret?t=deadbeef",
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg.LogSafe(logger)

	out := buf.String()
	if strings.Contains(out, cfg.TelegramBotToken) {
		t.Errorf("text output leaks TelegramBotToken: %s", out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "deadbeef") {
		t.Errorf("text output leaks webhook path/query: %s", out)
	}
	if !strings.Contains(out, "****") {
		t.Errorf("expected masked marker in output, got %s", out)
	}
}
