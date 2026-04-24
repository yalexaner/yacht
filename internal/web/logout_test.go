package web

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
)

// newLogoutTestServer builds a Server wired for the POST /logout route: a
// real migrated SQLite so auth.DeleteSession can actually remove rows, and
// production-default cookie config so the Set-Cookie assertions catch
// regressions in cookie scope. The auth providers themselves aren't
// exercised by /logout, but Server.New requires non-nil fields only when
// callers exercise those routes — leaving them nil here keeps the harness
// narrow.
func newLogoutTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "web-logout.db")
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

// insertLogoutTestUser inserts an admin users row so CreateSession has a
// valid foreign-key target. telegram_id uses wall-clock nanos to avoid
// collisions with other tests that share the same migrated schema.
func insertLogoutTestUser(t *testing.T, handle *sql.DB) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 1, strftime('%s','now'))`,
		time.Now().UnixNano(), "logout-user", "Logout User",
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

// TestLogout_HappyPath: a logged-in user POSTs /logout with a valid session
// cookie. The handler deletes the session row, emits a clearing Set-Cookie
// (MaxAge=-1, empty value) for yacht_session, and 303-redirects to /login.
// A subsequent hit on any authed route would see no cookie and be bounced
// back to /login — but that's the middleware's concern in Task 10.
func TestLogout_HappyPath(t *testing.T) {
	srv, handle := newLogoutTestServer(t)
	userID := insertLogoutTestUser(t, handle)

	sessionID, err := auth.CreateSession(context.Background(), handle, userID, "telegram_widget", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}

	c := findCookie(rec, "yacht_session")
	if c == nil {
		t.Fatalf("clearing cookie not set; Set-Cookie headers: %v", rec.Header().Values("Set-Cookie"))
	}
	if c.Value != "" {
		t.Errorf("cookie value: want empty (cleared), got %q", c.Value)
	}
	if c.MaxAge != -1 {
		t.Errorf("cookie MaxAge: want -1 (clear), got %d", c.MaxAge)
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

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, sessionID,
	).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("session row count after logout: want 0, got %d", count)
	}
}

// TestLogout_NoCookie: a POST /logout with no session cookie still redirects
// to /login and still emits a clearing Set-Cookie. Idempotency matters here
// because a stale browser tab or a user who already logged out in another
// tab will hit this endpoint without a cookie — any error response would
// be a worse UX than just redirecting them to the login page.
func TestLogout_NoCookie(t *testing.T) {
	srv, handle := newLogoutTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}

	c := findCookie(rec, "yacht_session")
	if c == nil {
		t.Fatalf("clearing cookie not set; Set-Cookie headers: %v", rec.Header().Values("Set-Cookie"))
	}
	if c.MaxAge != -1 {
		t.Errorf("cookie MaxAge: want -1 (clear), got %d", c.MaxAge)
	}

	// DB must be untouched — no sessions created, no sessions deleted.
	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions`,
	).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("sessions table should be empty; got %d rows", count)
	}
}

// TestLogout_UnknownSessionID: a POST /logout with a session cookie whose
// value doesn't match any row still clears the cookie and redirects.
// auth.DeleteSession is idempotent on missing rows, so this case is indistinguishable
// from TestLogout_NoCookie from the client's perspective. Matters because an
// attacker who guesses a session ID cannot tell from the response whether
// the session existed.
func TestLogout_UnknownSessionID(t *testing.T) {
	srv, _ := newLogoutTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: "not-a-real-session-id"})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
	c := findCookie(rec, "yacht_session")
	if c == nil || c.MaxAge != -1 {
		t.Errorf("expected clearing cookie with MaxAge=-1; got %+v", c)
	}
}

