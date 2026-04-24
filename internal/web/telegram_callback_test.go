package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
)

// telegramCallbackTestBotToken is the widget HMAC key used by
// newTelegramCallbackTestServer. Kept distinct from other test fixtures so a
// cross-package rename can't accidentally tie this file's assertions to
// values produced elsewhere.
const telegramCallbackTestBotToken = "callback-test:BOT-TOKEN-7890"

// newTelegramCallbackTestServer builds a Server with a real *sql.DB (post-
// migrations), a live auth.TelegramWidget bound to the test bot token, and a
// fully-populated config (SessionCookieName, SessionLifetime, BotUsername)
// so the handler's Set-Cookie matches what production would emit.
func newTelegramCallbackTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "web-auth.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	cfg := &config.Web{
		Shared:              &config.Shared{},
		SessionCookieName:   "yacht_session",
		SessionLifetime:     30 * 24 * time.Hour,
		TelegramBotUsername: "yachtshare_bot",
		TelegramBotToken:    telegramCallbackTestBotToken,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	widget := auth.NewTelegramWidget(handle, telegramCallbackTestBotToken)

	srv, err := New(cfg, handle, nil, widget, nil, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, handle
}

// insertCallbackTestUser inserts a users row with the given telegram_id and
// is_admin flag and returns the row's primary key. Mirrors the auth package's
// helper but lives here so this test file doesn't depend on unexported
// helpers from a sibling package.
func insertCallbackTestUser(t *testing.T, handle *sql.DB, telegramID int64, isAdmin bool) int64 {
	t.Helper()
	adminFlag := 0
	if isAdmin {
		adminFlag = 1
	}
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, ?, strftime('%s','now'))`,
		telegramID, "callback-user", "Callback User", adminFlag,
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// buildSignedWidgetQuery computes the Telegram widget hash the way Telegram
// does and returns the corresponding query string (no leading "?"). Kept as
// an independent implementation of the HMAC algorithm — if someone later
// breaks the production signer, the tests still verify against the spec.
func buildSignedWidgetQuery(t *testing.T, botToken string, fields map[string]string) string {
	t.Helper()

	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "hash" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+fields[k])
	}
	dataCheckString := strings.Join(lines, "\n")

	secret := sha256.Sum256([]byte(botToken))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write([]byte(dataCheckString))
	hash := hex.EncodeToString(mac.Sum(nil))

	vals := url.Values{}
	for k, v := range fields {
		vals.Set(k, v)
	}
	vals.Set("hash", hash)
	return vals.Encode()
}

// TestTelegramCallback_HappyPath: a valid widget URL for an admin user lands
// the browser back at "/" via 303, sets the yacht_session cookie with
// production-shaped attributes (HttpOnly, Lax, Path=/, matching MaxAge), and
// persists a sessions row the middleware will later consume.
func TestTelegramCallback_HappyPath(t *testing.T) {
	srv, handle := newTelegramCallbackTestServer(t)
	const tgID int64 = 7001
	adminID := insertCallbackTestUser(t, handle, tgID, true)

	query := buildSignedWidgetQuery(t, telegramCallbackTestBotToken, map[string]string{
		"id":         strconv.FormatInt(tgID, 10),
		"first_name": "Cal",
		"last_name":  "Callback",
		"username":   "cal",
		"auth_date":  strconv.FormatInt(time.Now().Unix(), 10),
	})

	req := httptest.NewRequest(http.MethodGet, "/auth/telegram/callback?"+query, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("location: want %q, got %q", "/", loc)
	}

	c := findCookie(rec, "yacht_session")
	if c == nil {
		t.Fatalf("session cookie not set; Set-Cookie headers: %v", rec.Header().Values("Set-Cookie"))
	}
	if c.Path != "/" {
		t.Errorf("cookie Path: want %q, got %q", "/", c.Path)
	}
	if !c.HttpOnly {
		t.Error("cookie HttpOnly: want true, got false")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite: want Lax (%d), got %d", http.SameSiteLaxMode, c.SameSite)
	}
	if c.Secure {
		// plain HTTP request in the test; requestIsTLS should return false.
		t.Error("cookie Secure: want false for plain HTTP, got true")
	}
	if wantMax := int((30 * 24 * time.Hour).Seconds()); c.MaxAge != wantMax {
		t.Errorf("cookie MaxAge: want %d (30d), got %d", wantMax, c.MaxAge)
	}
	if len(c.Value) != 64 {
		t.Errorf("cookie value length: want 64 (hex session id), got %d (%q)", len(c.Value), c.Value)
	}

	// the session row must exist, be tied to the admin user, and carry the
	// provider we expect so the cleanup worker and logout can find it.
	var (
		rowUserID   int64
		rowProvider string
		rowExpires  int64
	)
	err := handle.QueryRowContext(context.Background(),
		`SELECT user_id, provider, expires_at FROM sessions WHERE id = ?`, c.Value,
	).Scan(&rowUserID, &rowProvider, &rowExpires)
	if err != nil {
		t.Fatalf("lookup session row: %v", err)
	}
	if rowUserID != adminID {
		t.Errorf("session user_id: want %d, got %d", adminID, rowUserID)
	}
	if rowProvider != "telegram_widget" {
		t.Errorf("session provider: want %q, got %q", "telegram_widget", rowProvider)
	}
	if rowExpires <= time.Now().Unix() {
		t.Errorf("session expires_at: want future, got %d (now=%d)", rowExpires, time.Now().Unix())
	}
}

// TestTelegramCallback_HappyPathSecureOverTLS: the session cookie must carry
// Secure when the request arrived over TLS (direct or via the reverse-proxy
// X-Forwarded-Proto header). Mirrors the per-share cookie's Secure coverage
// — a leaked session cookie on plain HTTP is a full account takeover, so
// this is exactly the kind of regression we need a test for.
func TestTelegramCallback_HappyPathSecureOverTLS(t *testing.T) {
	srv, handle := newTelegramCallbackTestServer(t)
	const tgID int64 = 7002
	insertCallbackTestUser(t, handle, tgID, true)

	query := buildSignedWidgetQuery(t, telegramCallbackTestBotToken, map[string]string{
		"id":        strconv.FormatInt(tgID, 10),
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})

	req := httptest.NewRequest(http.MethodGet, "/auth/telegram/callback?"+query, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	c := findCookie(rec, "yacht_session")
	if c == nil {
		t.Fatalf("session cookie not set")
	}
	if !c.Secure {
		t.Error("cookie Secure: want true for X-Forwarded-Proto=https, got false")
	}
}

// TestTelegramCallback_InvalidSignature: a tampered hash lands on the login
// page with ?error=invalid_signature, and crucially no session row is
// created — every failed verify must leave the DB untouched.
func TestTelegramCallback_InvalidSignature(t *testing.T) {
	srv, handle := newTelegramCallbackTestServer(t)
	const tgID int64 = 7003
	insertCallbackTestUser(t, handle, tgID, true)

	query := buildSignedWidgetQuery(t, telegramCallbackTestBotToken, map[string]string{
		"id":        strconv.FormatInt(tgID, 10),
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})
	// flip one hex char of the hash — still valid hex, still right length,
	// but no longer matches the HMAC.
	vals, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	h := []byte(vals.Get("hash"))
	if h[0] == '0' {
		h[0] = '1'
	} else {
		h[0] = '0'
	}
	vals.Set("hash", string(h))

	req := httptest.NewRequest(http.MethodGet, "/auth/telegram/callback?"+vals.Encode(), nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=invalid_signature" {
		t.Errorf("location: want %q, got %q", "/login?error=invalid_signature", loc)
	}
	if c := findCookie(rec, "yacht_session"); c != nil {
		t.Errorf("session cookie must NOT be set on invalid signature; got %+v", c)
	}

	// no session row should have been inserted.
	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions`,
	).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("session row count: want 0, got %d", count)
	}
}

// TestTelegramCallback_AccessDenied: a valid signature for a user who exists
// but is not an admin lands on the login page with ?error=access_denied. The
// signature layer did its job; the allowlist step (Phase 9 is admin-only)
// rejected the user. No session row either.
func TestTelegramCallback_AccessDenied(t *testing.T) {
	srv, handle := newTelegramCallbackTestServer(t)
	const tgID int64 = 7004
	insertCallbackTestUser(t, handle, tgID, false)

	query := buildSignedWidgetQuery(t, telegramCallbackTestBotToken, map[string]string{
		"id":        strconv.FormatInt(tgID, 10),
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})

	req := httptest.NewRequest(http.MethodGet, "/auth/telegram/callback?"+query, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=access_denied" {
		t.Errorf("location: want %q, got %q", "/login?error=access_denied", loc)
	}
	if c := findCookie(rec, "yacht_session"); c != nil {
		t.Errorf("session cookie must NOT be set on access denied; got %+v", c)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions`,
	).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("session row count: want 0, got %d", count)
	}
}

// TestTelegramCallback_UnknownUser: a valid signature for a Telegram ID with
// no matching users row collapses into ErrUnauthorized the same way a non-
// admin does. Phase 9 deliberately does not distinguish "unknown" from
// "non-admin" — both are "access denied" to the user.
func TestTelegramCallback_UnknownUser(t *testing.T) {
	srv, _ := newTelegramCallbackTestServer(t)

	query := buildSignedWidgetQuery(t, telegramCallbackTestBotToken, map[string]string{
		"id":        "99999",
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})

	req := httptest.NewRequest(http.MethodGet, "/auth/telegram/callback?"+query, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=access_denied" {
		t.Errorf("location: want %q, got %q", "/login?error=access_denied", loc)
	}
}
