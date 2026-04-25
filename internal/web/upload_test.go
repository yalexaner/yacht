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

// newUploadTestServer builds a Server backed by a real *sql.DB so the
// RequireAuth gate the upload routes ride behind can resolve session
// cookies. DefaultExpiry is fixed at 24h so the form's pre-selected option
// is the canonical one; MaxUploadBytes is small enough that an oversized-body
// regression test would catch a missing MaxBytesReader without churning real
// megabytes through the recorder.
func newUploadTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "upload.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	cfg := &config.Web{
		Shared: &config.Shared{
			DefaultExpiry:  24 * time.Hour,
			MaxUploadBytes: 1024 * 1024,
		},
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

// insertUploadTestAdmin inserts an admin users row so CreateSession's
// downstream RequireAuth lookup (which enforces is_admin=1) can succeed.
// telegram_id uses wall-clock nanos to avoid the UNIQUE constraint colliding
// across tests in the same process.
func insertUploadTestAdmin(t *testing.T, handle *sql.DB) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 1, strftime('%s','now'))`,
		time.Now().UnixNano(), "uploader", "Uploader",
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

// uploadTestSession mints a real session row for the user and returns the
// cookie value. Tests that want to exercise an authenticated request thread
// it through req.AddCookie so RequireAuth resolves it the same way the
// production middleware would.
func uploadTestSession(t *testing.T, handle *sql.DB, userID int64) string {
	t.Helper()
	sessionID, err := auth.CreateSession(
		context.Background(), handle, userID, "test", 30*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sessionID
}

// TestUploadForm_RequiresAuth proves GET /upload is wired behind the
// RequireAuth middleware: a request without a session cookie must redirect
// to /login (303) rather than render the form. Without this guard, a
// routing change that drops the gate would silently leak the upload form to
// the public.
func TestUploadForm_RequiresAuth(t *testing.T) {
	srv, _ := newUploadTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
}

// TestUploadForm_RendersForm exercises the happy path: with a valid session,
// GET /upload renders the form with the kind radio, password input, expiry
// select carrying all six allowlist options, the text area, and the file
// input. Substring matching keeps Phase 14 styling tweaks from breaking the
// test, while still pinning the structural pieces handler logic relies on.
func TestUploadForm_RendersForm(t *testing.T) {
	srv, handle := newUploadTestServer(t)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`method="post"`,
		`action="/upload"`,
		`enctype="multipart/form-data"`,
		`name="kind"`,
		`value="file"`,
		`value="text"`,
		`name="password"`,
		`name="expiry"`,
		`name="text"`,
		`name="file"`,
		`type="file"`,
		`<textarea`,
		`<select`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	for _, secs := range []string{"3600", "21600", "86400", "259200", "604800", "2592000"} {
		if !strings.Contains(body, `value="`+secs+`"`) {
			t.Errorf("body missing expiry option value=%q; got:\n%s", secs, body)
		}
	}
	// the 24h option must be pre-selected against DefaultExpiry=24h.
	if !strings.Contains(body, `value="86400" selected`) {
		t.Errorf("body missing pre-selected 24h option; got:\n%s", body)
	}
}

// TestUploadForm_FieldOrder pins the load-bearing order from decision #2:
// non-file fields (kind, password, expiry, text) must precede the file
// input in the rendered HTML so they arrive first in the multipart stream.
// A future template tweak that re-orders these would silently break the
// streaming POST handler, so the regression guard lives here.
func TestUploadForm_FieldOrder(t *testing.T) {
	srv, handle := newUploadTestServer(t)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	body := rec.Body.String()
	order := []string{`name="kind"`, `name="password"`, `name="expiry"`, `name="text"`, `name="file"`}
	pos := -1
	for _, marker := range order {
		next := strings.Index(body, marker)
		if next < 0 {
			t.Fatalf("body missing %q; got:\n%s", marker, body)
		}
		if next <= pos {
			t.Errorf("field %q out of order: index %d not after previous %d", marker, next, pos)
		}
		pos = next
	}
}

// TestDefaultExpirySeconds covers the unit-level fallback: an unrecognized
// configured DefaultExpiry must collapse to 86400 (24h) so the dropdown
// always has a selected option. Keeps the helper honest independently of
// the template so a future refactor can move the call site without losing
// the guarantee.
func TestDefaultExpirySeconds(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want int64
	}{
		{"matches 1h option", 1 * time.Hour, 3600},
		{"matches 24h option", 24 * time.Hour, 86400},
		{"matches 30d option", 30 * 24 * time.Hour, 2592000},
		{"unmatched falls back to 24h", 5 * time.Minute, 86400},
		{"zero falls back to 24h", 0, 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultExpirySeconds(tc.in); got != tc.want {
				t.Errorf("defaultExpirySeconds(%v): want %d, got %d", tc.in, tc.want, got)
			}
		})
	}
}
