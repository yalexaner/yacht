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

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/storage/local"
)

// newCreatedTestServer wires the same full-stack DB + storage + share service
// as the upload tests, but with a fixed BaseURL so the createdHandler's
// rendered share URL is predictable. Returns the *share.Service so tests can
// mint real share rows directly without going through the POST /upload flow.
func newCreatedTestServer(t *testing.T) (*Server, *share.Service, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "created.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	shared := &config.Shared{
		BaseURL:        "https://send.example.com",
		DefaultExpiry:  24 * time.Hour,
		MaxUploadBytes: 1024 * 1024,
	}
	svc := share.New(handle, backend, shared)

	cfg := &config.Web{
		Shared:            shared,
		SessionCookieName: "yacht_session",
		SessionLifetime:   30 * 24 * time.Hour,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := New(cfg, handle, svc, nil, nil, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, svc, handle
}

// insertCreatedTestAdmin mints an admin users row (is_admin=1) so the
// RequireAuth middleware's lookup succeeds for the resulting session. Two
// callers in one test get distinct telegram_ids via the supplied seed so the
// UNIQUE constraint on telegram_id holds across both rows.
func insertCreatedTestAdmin(t *testing.T, handle *sql.DB, seed int64) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 1, strftime('%s','now'))`,
		time.Now().UnixNano()+seed, "creator", "Creator",
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

// TestCreated_RequiresAuth: GET /shares/{id}/created without a session
// cookie must redirect to /login (303) — the RequireAuth gate is the only
// thing keeping share IDs out of the hands of unauthenticated visitors who
// happen to know one.
func TestCreated_RequiresAuth(t *testing.T) {
	srv, _, _ := newCreatedTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/shares/anything/created", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
}

// TestCreated_HappyPath_File: a freshly-created file share rendered through
// /shares/{id}/created surfaces the absolute share URL (BaseURL + "/" + id),
// the original filename, and a humanized size — the three pieces a user
// reaches for after a successful upload.
func TestCreated_HappyPath_File(t *testing.T) {
	srv, svc, handle := newCreatedTestServer(t)
	userID := insertCreatedTestAdmin(t, handle, 1)
	sessionID := uploadTestSession(t, handle, userID)

	payload := []byte("file payload bytes")
	sh, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "report.pdf",
		MIMEType:         "application/pdf",
		Size:             int64(len(payload)),
		Content:          strings.NewReader(string(payload)),
		Expiry:           time.Hour,
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/shares/"+sh.ID+"/created", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"https://send.example.com/" + sh.ID,
		"report.pdf",
		"data-copy-text=",
		"Copy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

// TestCreated_HappyPath_Text: a text share's confirmation page surfaces the
// share URL plus a "View text share" link pointing at /{id} so the operator
// can preview the rendered text without leaving the flow. Filename + size
// blocks are absent for text shares — the template branches on Kind.
func TestCreated_HappyPath_Text(t *testing.T) {
	srv, svc, handle := newCreatedTestServer(t)
	userID := insertCreatedTestAdmin(t, handle, 1)
	sessionID := uploadTestSession(t, handle, userID)

	sh, err := svc.CreateTextShare(context.Background(), share.CreateTextOpts{
		UserID:  userID,
		Content: "a secret memo",
		Expiry:  time.Hour,
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/shares/"+sh.ID+"/created", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"https://send.example.com/" + sh.ID,
		`href="/` + sh.ID + `"`,
		"View text share",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

// TestCreated_NotOwner: a share owned by user A must surface as 404 to user
// B's session — the same response shape as a missing share, so a probing
// visitor cannot enumerate share IDs through this route. Returning 403 here
// would leak existence; 404 keeps the oracle closed.
func TestCreated_NotOwner(t *testing.T) {
	srv, svc, handle := newCreatedTestServer(t)
	ownerID := insertCreatedTestAdmin(t, handle, 1)
	intruderID := insertCreatedTestAdmin(t, handle, 2)
	intruderSession := uploadTestSession(t, handle, intruderID)

	sh, err := svc.CreateTextShare(context.Background(), share.CreateTextOpts{
		UserID:  ownerID,
		Content: "owner-only",
		Expiry:  time.Hour,
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/shares/"+sh.ID+"/created", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: intruderSession})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", rec.Code)
	}
	body := rec.Body.String()
	// the share URL must not appear — the page must look exactly like a
	// missing-share 404 to the intruder, no leak of the secret content.
	if strings.Contains(body, sh.ID) {
		t.Errorf("body leaks share id %q to non-owner; got:\n%s", sh.ID, body)
	}
}

// TestCreated_Missing: a share id that doesn't exist in the DB must surface
// as 404 with the standard error template — same mapping as shareHandler so
// the user-facing behaviour is consistent across the share routes.
func TestCreated_Missing(t *testing.T) {
	srv, _, handle := newCreatedTestServer(t)
	userID := insertCreatedTestAdmin(t, handle, 1)
	sessionID := uploadTestSession(t, handle, userID)

	req := httptest.NewRequest(http.MethodGet, "/shares/no-such-id/created", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", rec.Code)
	}
}

// TestCreated_Expired: a share whose expires_at has already passed maps to
// 410 Gone — the row exists, but the share is dead. Same mapping as the
// public share page; the operator's POV after their share has lapsed is
// indistinguishable from a recipient hitting the link past its TTL.
func TestCreated_Expired(t *testing.T) {
	srv, _, handle := newCreatedTestServer(t)
	userID := insertCreatedTestAdmin(t, handle, 1)
	sessionID := uploadTestSession(t, handle, userID)

	// Insert an already-expired text share row directly: the service won't
	// let us mint one with a past expires_at, but a row left over from a
	// prior creation that has since lapsed is exactly the case we need to
	// cover.
	id := "expired1"
	now := time.Now().Add(-2 * time.Hour).Unix()
	_, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO shares (id, user_id, kind, text_content, created_at, expires_at)
		 VALUES (?, ?, 'text', 'gone', ?, ?)`,
		id, userID, now, now+60, // expired ~119 minutes ago
	)
	if err != nil {
		t.Fatalf("insert expired share: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/shares/"+id+"/created", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status: want 410, got %d", rec.Code)
	}
}
