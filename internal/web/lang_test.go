package web

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
)

// langTestSession mints a real session row for the user via the
// telegram_widget provider (the only one auth.CreateSession's CHECK
// constraint accepts in tests that don't actually run the bot-token flow)
// and returns the cookie value. The provider name is irrelevant for
// langHandler — only the session→user resolution matters — but it must
// satisfy the sessions table CHECK constraint to insert at all.
func langTestSession(t *testing.T, handle *sql.DB, userID int64) string {
	t.Helper()
	sessionID, err := auth.CreateSession(
		context.Background(), handle, userID, "telegram_widget", 30*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sessionID
}

// newLangTestServer wires a Server backed by a real SQLite handle so
// langHandler can read users / sessions through auth.GetSession and write
// users.lang. No share service or storage backend — the lang route doesn't
// touch them. SessionCookieName matches the production default so the
// inline session lookup picks the same cookie name production handlers
// would use.
func newLangTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lang.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	cfg := &config.Web{
		Shared:            &config.Shared{},
		SessionCookieName: "yacht_session",
		SessionLifetime:   30 * 24 * time.Hour,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := New(cfg, handle, nil, nil, nil, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, handle
}

// insertLangTestAdmin mints an admin users row whose lang column is initially
// NULL. The seed differentiator avoids the UNIQUE telegram_id constraint when
// a single test mints more than one user.
func insertLangTestAdmin(t *testing.T, handle *sql.DB, seed int64) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 1, strftime('%s','now'))`,
		time.Now().UnixNano()+seed, "switcher", "Switcher",
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

// readUserLang returns users.lang for the given id as (value, isNull).
// Tests that want to assert "NULL is preserved" check the bool; tests that
// want to assert "lang persisted" check the string.
func readUserLang(t *testing.T, handle *sql.DB, userID int64) (string, bool) {
	t.Helper()
	var lang sql.NullString
	if err := handle.QueryRowContext(
		context.Background(),
		`SELECT lang FROM users WHERE id = ?`, userID,
	).Scan(&lang); err != nil {
		t.Fatalf("read users.lang: %v", err)
	}
	return lang.String, !lang.Valid
}

// TestLang_HappyPath_Anonymous: an unauthenticated visitor flipping the
// switcher gets the cookie set with the right attributes and a 303 redirect
// to "/". No DB write because there's no user to persist against.
func TestLang_HappyPath_Anonymous(t *testing.T) {
	srv, _ := newLangTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/lang/ru", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("location: want %q, got %q", "/", loc)
	}
	c := findCookie(rec, "yacht_lang")
	if c == nil {
		t.Fatalf("yacht_lang cookie not set; Set-Cookie headers: %v", rec.Header().Values("Set-Cookie"))
	}
	if c.Value != "ru" {
		t.Errorf("cookie value: want %q, got %q", "ru", c.Value)
	}
	if c.Path != "/" {
		t.Errorf("cookie path: want %q, got %q", "/", c.Path)
	}
	if !c.HttpOnly {
		t.Errorf("cookie HttpOnly: want true, got false")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite: want Lax (%d), got %d", http.SameSiteLaxMode, c.SameSite)
	}
	wantMaxAge := int((365 * 24 * time.Hour).Seconds())
	if c.MaxAge != wantMaxAge {
		t.Errorf("cookie MaxAge: want %d, got %d", wantMaxAge, c.MaxAge)
	}
	// no session cookie on the request → no opportunity to write users.lang.
	// The Secure flag is off because the recorder defaults to plain HTTP.
	if c.Secure {
		t.Errorf("cookie Secure: want false on plain HTTP, got true")
	}
}

// TestLang_HappyPath_Authenticated: an authenticated visitor's pick is
// mirrored into users.lang so the preference survives a cookie clear.
// The cookie is still set on top of the DB write — both surfaces stay in
// sync after a successful switch.
func TestLang_HappyPath_Authenticated(t *testing.T) {
	srv, handle := newLangTestServer(t)
	userID := insertLangTestAdmin(t, handle, 1)
	sessionID := langTestSession(t, handle, userID)

	// pre-condition: lang starts NULL ("never picked").
	if _, isNull := readUserLang(t, handle, userID); !isNull {
		t.Fatalf("precondition: users.lang should start NULL")
	}

	req := httptest.NewRequest(http.MethodGet, "/lang/ru", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if c := findCookie(rec, "yacht_lang"); c == nil || c.Value != "ru" {
		t.Errorf("yacht_lang cookie: want set to %q, got %+v", "ru", c)
	}
	got, isNull := readUserLang(t, handle, userID)
	if isNull {
		t.Fatalf("users.lang: want %q, got NULL", "ru")
	}
	if got != "ru" {
		t.Errorf("users.lang: want %q, got %q", "ru", got)
	}
}

// TestLang_UnknownCode: a code outside the {en, ru} allowlist must produce
// 400 with no cookie set and no DB write. Reject at the handler keeps junk
// out of the cookie jar entirely instead of relying on the middleware's
// IsSupported guard to ignore it later.
func TestLang_UnknownCode(t *testing.T) {
	srv, handle := newLangTestServer(t)
	userID := insertLangTestAdmin(t, handle, 1)
	sessionID := langTestSession(t, handle, userID)

	req := httptest.NewRequest(http.MethodGet, "/lang/zh", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if c := findCookie(rec, "yacht_lang"); c != nil {
		t.Errorf("yacht_lang cookie should NOT be set on unknown code; got %+v", c)
	}
	if _, isNull := readUserLang(t, handle, userID); !isNull {
		t.Errorf("users.lang should remain NULL on unknown code")
	}
}

// TestLang_UnknownCode_RendersRussianError: a yacht_lang=ru cookie present
// on the request must steer the 400 error page through the Russian bundle
// — even though the requested target language ("zh") was rejected. Locks
// in Task 7's renderError → bundle-key wiring on a path that's easy to
// drive without minting a session: an existing language preference must
// survive a junk switch attempt.
func TestLang_UnknownCode_RendersRussianError(t *testing.T) {
	srv, _ := newLangTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/lang/zh", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_lang", Value: "ru"})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// "Неподдерживаемый язык." is the RU translation of
	// error.badrequest.unsupportedlang. Substring-asserting the trailing
	// dot would be brittle if the period ever moves into the bundle; the
	// body is enough.
	if !strings.Contains(body, "Неподдерживаемый язык") {
		t.Errorf("body missing Russian unsupported-language message; got:\n%s", body)
	}
	// "Некорректный запрос" is the RU translation of error.badrequest.title.
	if !strings.Contains(body, "Некорректный запрос") {
		t.Errorf("body missing Russian error title; got:\n%s", body)
	}
	// Defense-in-depth: bundle keys must never leak through as text.
	if strings.Contains(body, "error.badrequest") {
		t.Errorf("body leaked bundle key as text; got:\n%s", body)
	}
}

// TestLang_DBWriteFails: a DB fault during the users.lang update must not
// fail the request. The visitor's primary intent — set the cookie, redirect
// — has already succeeded, and surfacing the DB error here would make a
// transient hiccup look like a broken language switcher. We trigger the
// failure by closing the DB handle before the request runs; GetSession
// fails inside userFromSessionCookie, so the UPDATE is never attempted but
// the redirect + cookie still go through.
func TestLang_DBWriteFails(t *testing.T) {
	srv, handle := newLangTestServer(t)
	userID := insertLangTestAdmin(t, handle, 1)
	sessionID := langTestSession(t, handle, userID)

	if err := handle.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/lang/ru", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 even with broken DB, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("location: want %q, got %q", "/", loc)
	}
	c := findCookie(rec, "yacht_lang")
	if c == nil || c.Value != "ru" {
		t.Errorf("yacht_lang cookie: want set to %q with broken DB, got %+v", "ru", c)
	}
	_ = userID // userID is only relevant for the precondition; broken DB makes a follow-up read pointless
}

// TestLang_RefererSameOrigin: a Referer header pointing at a path on the
// same host is honoured as the redirect target so the user lands back where
// they came from. The host comparison uses r.Host (the Host header on the
// recorded request), so the test's same-host setup can rely on the default
// httptest.NewRequest host of "example.com".
func TestLang_RefererSameOrigin(t *testing.T) {
	srv, _ := newLangTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/lang/ru", nil)
	req.Header.Set("Referer", "http://"+req.Host+"/upload")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/upload" {
		t.Errorf("location: want %q, got %q", "/upload", loc)
	}
}

// TestLang_RefererCrossOrigin: a Referer pointing at an external host must
// collapse to "/" so the lang endpoint cannot be used as a generic
// open-redirect. An attacker who lures a logged-in user into clicking
// /lang/ru from an external page should not be able to bounce them
// anywhere via the Referer header.
func TestLang_RefererCrossOrigin(t *testing.T) {
	srv, _ := newLangTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/lang/ru", nil)
	req.Header.Set("Referer", "http://evil.com/phishy/path")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("location: want %q (open-redirect guard), got %q", "/", loc)
	}
}

// TestLang_SecureFlagOverTLS: the cookie carries Secure when the request
// arrived over TLS — directly via r.TLS or indirectly via
// X-Forwarded-Proto=https from a TLS-terminating proxy. Mirrors the
// per-share unlock cookie's TLS contract so the lang preference cookie has
// the same on-path-attacker resistance.
func TestLang_SecureFlagOverTLS(t *testing.T) {
	srv, _ := newLangTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/lang/ru", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	c := findCookie(rec, "yacht_lang")
	if c == nil {
		t.Fatalf("yacht_lang cookie not set")
	}
	if !c.Secure {
		t.Errorf("cookie Secure: want true under X-Forwarded-Proto=https, got false")
	}
}
