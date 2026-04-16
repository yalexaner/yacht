package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// validWebEnv extends a valid Shared r2 env with every web-only var, ready
// for individual tests to mutate.
func validWebEnv() map[string]string {
	env := validSharedR2()
	env["HTTP_LISTEN"] = "0.0.0.0:9090"
	env["SESSION_COOKIE_NAME"] = "yacht_s"
	env["SESSION_LIFETIME_DAYS"] = "7"
	env["TELEGRAM_BOT_USERNAME"] = "yachtshare_bot"
	env["TELEGRAM_BOT_TOKEN"] = "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	return env
}

func TestLoadWeb_HappyPath(t *testing.T) {
	setSharedEnv(t, validWebEnv())
	cfg, err := LoadWeb()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.Shared == nil {
		t.Fatal("Shared should be populated")
	}
	if cfg.HTTPListen != "0.0.0.0:9090" {
		t.Errorf("HTTPListen = %q", cfg.HTTPListen)
	}
	if cfg.SessionCookieName != "yacht_s" {
		t.Errorf("SessionCookieName = %q", cfg.SessionCookieName)
	}
	if cfg.SessionLifetime != 7*24*time.Hour {
		t.Errorf("SessionLifetime = %v, want 7d", cfg.SessionLifetime)
	}
	if cfg.TelegramBotUsername != "yachtshare_bot" {
		t.Errorf("TelegramBotUsername = %q", cfg.TelegramBotUsername)
	}
	if cfg.TelegramBotToken != "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Errorf("TelegramBotToken = %q", cfg.TelegramBotToken)
	}
	// a shared field must also be reachable via the embedded pointer.
	if cfg.BaseURL != "https://send.example.com" {
		t.Errorf("embedded BaseURL = %q", cfg.BaseURL)
	}
}

func TestLoadWeb_DefaultsApplied(t *testing.T) {
	env := validWebEnv()
	delete(env, "HTTP_LISTEN")
	delete(env, "SESSION_COOKIE_NAME")
	delete(env, "SESSION_LIFETIME_DAYS")
	setSharedEnv(t, env)

	cfg, err := LoadWeb()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.HTTPListen != defaultHTTPListen {
		t.Errorf("HTTPListen default = %q, want %q", cfg.HTTPListen, defaultHTTPListen)
	}
	if cfg.SessionCookieName != defaultSessionCookieName {
		t.Errorf("SessionCookieName default = %q, want %q", cfg.SessionCookieName, defaultSessionCookieName)
	}
	if cfg.SessionLifetime != defaultSessionLifetimeDays*24*time.Hour {
		t.Errorf("SessionLifetime default = %v, want %dd", cfg.SessionLifetime, defaultSessionLifetimeDays)
	}
}

func TestLoadWeb_MissingRequiredWebVars(t *testing.T) {
	env := validWebEnv()
	env["TELEGRAM_BOT_USERNAME"] = ""
	env["TELEGRAM_BOT_TOKEN"] = ""
	setSharedEnv(t, env)

	_, err := LoadWeb()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	for _, name := range []string{"TELEGRAM_BOT_USERNAME", "TELEGRAM_BOT_TOKEN"} {
		if !strings.Contains(msg, name) {
			t.Errorf("err should mention %s, got %q", name, msg)
		}
	}
}

func TestLoadWeb_AggregatesSharedAndWebErrors(t *testing.T) {
	// blank a required shared var AND a required web var; the joined error
	// must list both so the operator sees every problem at once.
	env := validWebEnv()
	env["BASE_URL"] = ""
	env["TELEGRAM_BOT_TOKEN"] = ""
	setSharedEnv(t, env)

	_, err := LoadWeb()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BASE_URL") {
		t.Errorf("err should mention BASE_URL (shared), got %q", msg)
	}
	if !strings.Contains(msg, "TELEGRAM_BOT_TOKEN") {
		t.Errorf("err should mention TELEGRAM_BOT_TOKEN (web), got %q", msg)
	}
}

func TestLoadWeb_MalformedSessionLifetime(t *testing.T) {
	env := validWebEnv()
	env["SESSION_LIFETIME_DAYS"] = "thirty"
	setSharedEnv(t, env)

	_, err := LoadWeb()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "SESSION_LIFETIME_DAYS") {
		t.Errorf("err should mention SESSION_LIFETIME_DAYS, got %q", err.Error())
	}
}

// TestLoadWeb_NonPositiveSessionLifetimeRejected locks in that zero or
// negative SESSION_LIFETIME_DAYS is rejected — either would invalidate
// sessions the instant they're created.
func TestLoadWeb_NonPositiveSessionLifetimeRejected(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run("value="+value, func(t *testing.T) {
			env := validWebEnv()
			env["SESSION_LIFETIME_DAYS"] = value
			setSharedEnv(t, env)

			_, err := LoadWeb()
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), "SESSION_LIFETIME_DAYS") {
				t.Errorf("err should mention SESSION_LIFETIME_DAYS, got %q", err.Error())
			}
			if !strings.Contains(err.Error(), "positive") {
				t.Errorf("err should mention %q, got %q", "positive", err.Error())
			}
		})
	}
}

func TestWebLogSafe_MasksTelegramToken(t *testing.T) {
	cfg := &Web{
		Shared: &Shared{
			BaseURL:        "https://send.example.com",
			StorageBackend: StorageBackendR2,
		},
		HTTPListen:          "127.0.0.1:8080",
		SessionCookieName:   "yacht_session",
		SessionLifetime:     30 * 24 * time.Hour,
		TelegramBotUsername: "yachtshare_bot",
		TelegramBotToken:    "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}

	h := &capturingHandler{}
	logger := slog.New(h)
	cfg.LogSafe(logger)

	if len(h.records) < 2 {
		t.Fatalf("expected at least 2 records (shared + web), got %d", len(h.records))
	}

	// find the web record by message name.
	var webRec *slog.Record
	for i := range h.records {
		if h.records[i].Message == "config.web" {
			webRec = &h.records[i]
			break
		}
	}
	if webRec == nil {
		t.Fatal("no config.web record was emitted")
	}
	attrs := attrMap(*webRec)

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
	// non-secret fields must be logged verbatim.
	if attrs["http_listen"] != cfg.HTTPListen {
		t.Errorf("http_listen = %q, want %q", attrs["http_listen"], cfg.HTTPListen)
	}
	if attrs["telegram_bot_username"] != cfg.TelegramBotUsername {
		t.Errorf("telegram_bot_username = %q, want %q", attrs["telegram_bot_username"], cfg.TelegramBotUsername)
	}
}

// TestWebLogSafe_TextHandlerSmoke confirms the masking promise holds when a
// real text handler renders the record.
func TestWebLogSafe_TextHandlerSmoke(t *testing.T) {
	cfg := &Web{
		Shared:           &Shared{StorageBackend: StorageBackendR2},
		TelegramBotToken: "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg.LogSafe(logger)

	out := buf.String()
	if strings.Contains(out, cfg.TelegramBotToken) {
		t.Errorf("text output leaks TelegramBotToken: %s", out)
	}
	if !strings.Contains(out, "****") {
		t.Errorf("expected masked marker in output, got %s", out)
	}
}
