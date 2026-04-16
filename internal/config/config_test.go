package config

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// setSharedEnv applies a map of env vars for a test, using t.Setenv so the
// values are torn down automatically. Keys with empty string values are
// written verbatim (allowing tests to override a prior set to ""); keys
// omitted from the map stay unset.
func setSharedEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// validSharedR2 returns a fully populated env map for the r2 backend. Tests
// mutate a copy to construct invalid/partial scenarios.
//
// The overridable numeric / enum values are intentionally NOT equal to the
// package defaults (DEFAULT_LANG, DEFAULT_EXPIRY_HOURS, MAX_UPLOAD_BYTES,
// STORAGE_BACKEND) so the happy-path test fails if LoadShared ever silently
// ignores the env and returns a default. Tests that care about default-path
// behaviour use a different fixture (see TestLoadShared_DefaultsApplied).
func validSharedR2() map[string]string {
	return map[string]string{
		"BASE_URL":             "https://send.example.com",
		"BRAND_URL":            "https://brand.example.com",
		"DB_PATH":              "/var/lib/yacht/meta.db",
		"DEFAULT_LANG":         "ru",
		"DEFAULT_EXPIRY_HOURS": "48",
		"MAX_UPLOAD_BYTES":     "52428800",
		"STORAGE_BACKEND":      "r2",
		"R2_ACCOUNT_ID":        "acct-123",
		"R2_ACCESS_KEY_ID":     "AKIDEXAMPLE1234567890",
		"R2_SECRET_ACCESS_KEY": "secret-ABCDEFGHIJKLMNOP",
		"R2_BUCKET":            "yacht-shares",
		"R2_ENDPOINT":          "https://acct.r2.cloudflarestorage.com",
	}
}

func validSharedLocal() map[string]string {
	return map[string]string{
		"BASE_URL":           "https://send.example.com",
		"BRAND_URL":          "https://brand.example.com",
		"DB_PATH":            "/var/lib/yacht/meta.db",
		"STORAGE_BACKEND":    "local",
		"STORAGE_LOCAL_PATH": "/var/lib/yacht/files",
	}
}

func TestLoadShared_HappyPathR2(t *testing.T) {
	setSharedEnv(t, validSharedR2())
	cfg, err := LoadShared()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.BaseURL != "https://send.example.com" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.BrandURL != "https://brand.example.com" {
		t.Errorf("BrandURL = %q", cfg.BrandURL)
	}
	if cfg.DBPath != "/var/lib/yacht/meta.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.DefaultLang != "ru" {
		t.Errorf("DefaultLang = %q", cfg.DefaultLang)
	}
	if cfg.DefaultExpiry != 48*time.Hour {
		t.Errorf("DefaultExpiry = %v", cfg.DefaultExpiry)
	}
	if cfg.MaxUploadBytes != 52428800 {
		t.Errorf("MaxUploadBytes = %d", cfg.MaxUploadBytes)
	}
	if cfg.StorageBackend != StorageBackendR2 {
		t.Errorf("StorageBackend = %q", cfg.StorageBackend)
	}
	if cfg.StorageLocalPath != "" {
		t.Errorf("StorageLocalPath should be empty on r2, got %q", cfg.StorageLocalPath)
	}
	if cfg.R2AccountID != "acct-123" {
		t.Errorf("R2AccountID = %q", cfg.R2AccountID)
	}
	if cfg.R2AccessKeyID != "AKIDEXAMPLE1234567890" {
		t.Errorf("R2AccessKeyID = %q", cfg.R2AccessKeyID)
	}
	if cfg.R2SecretAccessKey != "secret-ABCDEFGHIJKLMNOP" {
		t.Errorf("R2SecretAccessKey = %q", cfg.R2SecretAccessKey)
	}
	if cfg.R2Bucket != "yacht-shares" {
		t.Errorf("R2Bucket = %q", cfg.R2Bucket)
	}
	if cfg.R2Endpoint != "https://acct.r2.cloudflarestorage.com" {
		t.Errorf("R2Endpoint = %q", cfg.R2Endpoint)
	}
}

func TestLoadShared_HappyPathLocal(t *testing.T) {
	setSharedEnv(t, validSharedLocal())
	cfg, err := LoadShared()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.StorageBackend != StorageBackendLocal {
		t.Errorf("StorageBackend = %q, want %q", cfg.StorageBackend, StorageBackendLocal)
	}
	if cfg.StorageLocalPath != "/var/lib/yacht/files" {
		t.Errorf("StorageLocalPath = %q", cfg.StorageLocalPath)
	}
	if cfg.R2AccountID != "" || cfg.R2Bucket != "" {
		t.Errorf("r2 fields should be empty on local backend")
	}
}

func TestLoadShared_DefaultsApplied(t *testing.T) {
	// Only the strictly-required shared vars plus a complete r2 block are
	// provided; DEFAULT_LANG / DEFAULT_EXPIRY_HOURS / MAX_UPLOAD_BYTES /
	// STORAGE_BACKEND are all left unset and must default.
	vars := map[string]string{
		"BASE_URL":             "https://send.example.com",
		"BRAND_URL":            "https://brand.example.com",
		"DB_PATH":              "/var/lib/yacht/meta.db",
		"R2_ACCOUNT_ID":        "acct",
		"R2_ACCESS_KEY_ID":     "AKID",
		"R2_SECRET_ACCESS_KEY": "secret",
		"R2_BUCKET":            "bucket",
		"R2_ENDPOINT":          "https://acct.r2.cloudflarestorage.com",
	}
	setSharedEnv(t, vars)
	cfg, err := LoadShared()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.DefaultLang != "en" {
		t.Errorf("DefaultLang default = %q, want %q", cfg.DefaultLang, "en")
	}
	if cfg.DefaultExpiry != 24*time.Hour {
		t.Errorf("DefaultExpiry default = %v, want 24h", cfg.DefaultExpiry)
	}
	if cfg.MaxUploadBytes != 104857600 {
		t.Errorf("MaxUploadBytes default = %d, want 104857600", cfg.MaxUploadBytes)
	}
	if cfg.StorageBackend != StorageBackendR2 {
		t.Errorf("StorageBackend default = %q, want %q", cfg.StorageBackend, StorageBackendR2)
	}
}

func TestLoadShared_InvalidBackend(t *testing.T) {
	vars := validSharedR2()
	vars["STORAGE_BACKEND"] = "floppy"
	setSharedEnv(t, vars)
	_, err := LoadShared()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "STORAGE_BACKEND") {
		t.Errorf("error should mention STORAGE_BACKEND, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "floppy") {
		t.Errorf("error should echo the invalid value, got %q", err.Error())
	}
}

func TestLoadShared_R2MissingVars(t *testing.T) {
	// start from a valid r2 env, then clear two of the five r2 vars. Both
	// missing names must appear in the aggregated error so the operator can
	// see every problem at once.
	vars := validSharedR2()
	vars["R2_ACCOUNT_ID"] = ""
	vars["R2_SECRET_ACCESS_KEY"] = ""
	setSharedEnv(t, vars)

	_, err := LoadShared()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "R2_ACCOUNT_ID") {
		t.Errorf("err should mention R2_ACCOUNT_ID, got %q", msg)
	}
	if !strings.Contains(msg, "R2_SECRET_ACCESS_KEY") {
		t.Errorf("err should mention R2_SECRET_ACCESS_KEY, got %q", msg)
	}
}

func TestLoadShared_LocalMissingPath(t *testing.T) {
	vars := validSharedLocal()
	vars["STORAGE_LOCAL_PATH"] = ""
	setSharedEnv(t, vars)
	_, err := LoadShared()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "STORAGE_LOCAL_PATH") {
		t.Errorf("err should mention STORAGE_LOCAL_PATH, got %q", err.Error())
	}
}

func TestLoadShared_MissingRequiredSharedVarsAreAllReported(t *testing.T) {
	// All three required shared vars are blank — a correctly aggregating
	// loader must list every missing var, not just the first one.
	vars := map[string]string{
		"BASE_URL":             "",
		"BRAND_URL":            "",
		"DB_PATH":              "",
		"STORAGE_BACKEND":      "r2",
		"R2_ACCOUNT_ID":        "acct",
		"R2_ACCESS_KEY_ID":     "AKID",
		"R2_SECRET_ACCESS_KEY": "secret",
		"R2_BUCKET":            "bucket",
		"R2_ENDPOINT":          "https://acct.r2.cloudflarestorage.com",
	}
	setSharedEnv(t, vars)

	_, err := LoadShared()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	for _, name := range []string{"BASE_URL", "BRAND_URL", "DB_PATH"} {
		if !strings.Contains(msg, name) {
			t.Errorf("err should mention %s, got %q", name, msg)
		}
	}
}

func TestLoadShared_MalformedURLs(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantSub string
	}{
		{name: "missing scheme", baseURL: "send.example.com", wantSub: "scheme"},
		{name: "missing host", baseURL: "https://", wantSub: "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := validSharedR2()
			vars["BASE_URL"] = tt.baseURL
			setSharedEnv(t, vars)
			_, err := LoadShared()
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), "BASE_URL") {
				t.Errorf("err should mention BASE_URL, got %q", err.Error())
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("err should mention %q, got %q", tt.wantSub, err.Error())
			}
		})
	}
}

func TestLoadShared_MalformedNumeric(t *testing.T) {
	t.Run("max upload bytes", func(t *testing.T) {
		vars := validSharedR2()
		vars["MAX_UPLOAD_BYTES"] = "huge"
		setSharedEnv(t, vars)
		_, err := LoadShared()
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if !strings.Contains(err.Error(), "MAX_UPLOAD_BYTES") {
			t.Errorf("err should mention MAX_UPLOAD_BYTES, got %q", err.Error())
		}
	})

	t.Run("default expiry hours", func(t *testing.T) {
		vars := validSharedR2()
		vars["DEFAULT_EXPIRY_HOURS"] = "24h"
		setSharedEnv(t, vars)
		_, err := LoadShared()
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if !strings.Contains(err.Error(), "DEFAULT_EXPIRY_HOURS") {
			t.Errorf("err should mention DEFAULT_EXPIRY_HOURS, got %q", err.Error())
		}
	})
}

// TestLoadShared_NonPositiveNumericRejected locks in that zero or negative
// values for the two numeric knobs are rejected: 0 or negative expiry would
// create already-expired shares, and 0 or negative MAX_UPLOAD_BYTES would
// effectively disable uploads.
func TestLoadShared_NonPositiveNumericRejected(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantSub string
	}{
		{name: "expiry zero", key: "DEFAULT_EXPIRY_HOURS", value: "0", wantSub: "DEFAULT_EXPIRY_HOURS"},
		{name: "expiry negative", key: "DEFAULT_EXPIRY_HOURS", value: "-1", wantSub: "DEFAULT_EXPIRY_HOURS"},
		{name: "max upload zero", key: "MAX_UPLOAD_BYTES", value: "0", wantSub: "MAX_UPLOAD_BYTES"},
		{name: "max upload negative", key: "MAX_UPLOAD_BYTES", value: "-42", wantSub: "MAX_UPLOAD_BYTES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := validSharedR2()
			vars[tt.key] = tt.value
			setSharedEnv(t, vars)
			_, err := LoadShared()
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("err should mention %s, got %q", tt.wantSub, err.Error())
			}
			if !strings.Contains(err.Error(), "positive") {
				t.Errorf("err should mention %q, got %q", "positive", err.Error())
			}
		})
	}
}

// capturingHandler is a minimal slog.Handler that records every attribute of
// every record it handles. Tests use it to assert what LogSafe actually
// emits to the logger.
type capturingHandler struct {
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(_ string) slog.Handler      { return h }

// attrMap collects the attributes of the given record into a flat map keyed
// by attribute name, for easy assertion.
func attrMap(r slog.Record) map[string]string {
	m := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	return m
}

func TestSharedLogSafe_MasksSecrets(t *testing.T) {
	cfg := &Shared{
		BaseURL:           "https://send.example.com",
		BrandURL:          "https://brand.example.com",
		DBPath:            "/var/lib/yacht/meta.db",
		DefaultLang:       "en",
		DefaultExpiry:     24 * time.Hour,
		MaxUploadBytes:    104857600,
		StorageBackend:    StorageBackendR2,
		R2AccountID:       "acct-123",
		R2AccessKeyID:     "AKIDEXAMPLE1234567890",
		R2SecretAccessKey: "secret-ABCDEFGHIJKLMNOP",
		R2Bucket:          "yacht-shares",
		R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
	}
	h := &capturingHandler{}
	logger := slog.New(h)
	cfg.LogSafe(logger)

	if len(h.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(h.records))
	}
	attrs := attrMap(h.records[0])

	// the key values themselves must never be present verbatim.
	for _, v := range attrs {
		if strings.Contains(v, cfg.R2AccessKeyID) {
			t.Errorf("attr %q contains full R2AccessKeyID", v)
		}
		if strings.Contains(v, cfg.R2SecretAccessKey) {
			t.Errorf("attr %q contains full R2SecretAccessKey", v)
		}
	}

	// Assert the exact masked form: "****" + last 4 chars of the input. A
	// prefix-only check would falsely pass a buggy mask that returns pure
	// "****" regardless of input, so lock in the revealed suffix too.
	if got, want := attrs["r2_access_key_id"], "****7890"; got != want {
		t.Errorf("r2_access_key_id = %q, want %q", got, want)
	}
	if got, want := attrs["r2_secret_access_key"], "****MNOP"; got != want {
		t.Errorf("r2_secret_access_key = %q, want %q", got, want)
	}

	// non-secret fields must be logged verbatim so operators can verify them.
	if attrs["base_url"] != cfg.BaseURL {
		t.Errorf("base_url = %q, want %q", attrs["base_url"], cfg.BaseURL)
	}
	if attrs["r2_account_id"] != cfg.R2AccountID {
		t.Errorf("r2_account_id should be unmasked, got %q", attrs["r2_account_id"])
	}
}

// TestSharedLogSafe_TextHandlerSmoke confirms the same masking promise holds
// when a real text handler renders the record (guards against a future
// handler swap that might accidentally format secrets differently).
func TestSharedLogSafe_TextHandlerSmoke(t *testing.T) {
	cfg := &Shared{
		R2AccessKeyID:     "AKIDEXAMPLE1234567890",
		R2SecretAccessKey: "secret-ABCDEFGHIJKLMNOP",
		StorageBackend:    StorageBackendR2,
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg.LogSafe(logger)

	out := buf.String()
	if strings.Contains(out, cfg.R2AccessKeyID) {
		t.Errorf("text output leaks R2AccessKeyID: %s", out)
	}
	if strings.Contains(out, cfg.R2SecretAccessKey) {
		t.Errorf("text output leaks R2SecretAccessKey: %s", out)
	}
	if !strings.Contains(out, "****") {
		t.Errorf("expected masked marker in output, got %s", out)
	}
}
