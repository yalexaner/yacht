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

// newHomeTestServer builds a Server backed by a real *sql.DB so the
// RequireAuth middleware that gates GET / has a session table to consult.
// Mirrors the cookie config the production binary ships so the cookie that
// tests attach is the cookie the middleware actually reads.
func newHomeTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "home.db")
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

// insertHomeTestAdmin inserts an admin users row so CreateSession's
// downstream GetSession lookup (which enforces is_admin=1 in Phase 9) can
// succeed against it. Returns the new row's primary key.
func insertHomeTestAdmin(t *testing.T, handle *sql.DB, displayName string) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 1, strftime('%s','now'))`,
		time.Now().UnixNano(), "home-user", displayName,
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

// TestHome_RendersForAuthenticatedUser: with a valid session cookie, GET /
// renders the home placeholder and includes the user's display name and a
// logout form. Locks in the post-login redirect target so the auth flows
// don't 404 on success.
func TestHome_RendersForAuthenticatedUser(t *testing.T) {
	srv, handle := newHomeTestServer(t)
	userID := insertHomeTestAdmin(t, handle, "Ada Lovelace")

	sessionID, err := auth.CreateSession(
		context.Background(), handle, userID, "telegram_widget", 30*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Logged in", "Ada Lovelace", `action="/logout"`, `href="/upload"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

// TestHome_RedirectsUnauthenticatedToLogin: without a session cookie, GET /
// must bounce to /login via the RequireAuth middleware. Without this
// regression guard, a routing change that drops the gate would silently
// leak the placeholder home to the public.
func TestHome_RedirectsUnauthenticatedToLogin(t *testing.T) {
	srv, _ := newHomeTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
}

