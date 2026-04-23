package web

import (
	"bytes"
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

// newTestServer builds a Server wired to the embedded template + static FS
// with a no-op logger and no share service. Used by the Task 2 tests that
// only exercise routes independent of share state (healthz, static).
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Web{Shared: &config.Shared{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, nil, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// newTestServerWithShare builds a Server backed by a fresh on-disk SQLite
// and a fresh local-storage backend, wired through a real share.Service.
// Returned handle + service let tests create shares (or insert expired rows
// directly) before exercising the HTTP handlers.
//
// Both backing directories use separate t.TempDir() calls so a test that
// inspects the filesystem doesn't see state from the other store.
func newTestServerWithShare(t *testing.T) (*Server, *share.Service, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
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

	shared := &config.Shared{DefaultExpiry: 24 * time.Hour}
	svc := share.New(handle, backend, shared)

	cfg := &config.Web{Shared: shared}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, svc, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, svc, handle
}

// insertWebTestUser inserts a users row and returns the new id. The share
// handlers don't need the user themselves, but CreateFileShare /
// CreateTextShare require a valid user_id thanks to the FK on shares.user_id.
// telegram_id uses wall-clock nanos so two users inside one test don't
// collide on the UNIQUE constraint.
func insertWebTestUser(t *testing.T, handle *sql.DB) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, is_admin, created_at)
		 VALUES (?, 0, strftime('%s','now'))`,
		time.Now().UnixNano(),
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

// TestNew_ParsesTemplates is the Phase-7 Task-1 sanity check, updated for
// Task 3's clone-per-page template scheme: Server.New walks the embedded
// templates FS at construction and produces one fully-assembled template
// per page basename (base.html merged with that page's content/title
// overrides). Losing one of these entries would silently break a render
// path the handlers rely on.
func TestNew_ParsesTemplates(t *testing.T) {
	srv := newTestServer(t)

	for _, name := range []string{
		"share_file.html",
		"share_text.html",
		"password.html",
		"error.html",
	} {
		tmpl, ok := srv.templates[name]
		if !ok {
			t.Errorf("template %q not parsed", name)
			continue
		}
		if tmpl.Lookup("base.html") == nil {
			t.Errorf("template %q missing base.html association", name)
		}
		if tmpl.Lookup("content") == nil {
			t.Errorf("template %q missing content block override", name)
		}
	}
}

// TestHealthz exercises the liveness probe: a 200 "ok\n" response with a
// text/plain content type. Keeping the contract explicit here means a
// future well-meaning refactor that swaps in structured JSON won't sneak
// past CI and break whatever health checker is polling this endpoint.
func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "ok\n" {
		t.Errorf("body: want %q, got %q", "ok\n", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type: want text/plain prefix, got %q", ct)
	}
}

// TestStatic_ServesCSS confirms that http.FileServer + StripPrefix resolve
// /static/style.css against the embedded FS. The assertion on body content
// pins a substring from the real CSS rather than the whole file — Phase
// 14 will iterate on styling, and pinning the full body would turn every
// CSS tweak into a test update.
func TestStatic_ServesCSS(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type: want text/css prefix, got %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "--accent") {
		t.Errorf("body: expected CSS content with --accent, got %q", body)
	}
}

// TestStatic_NonexistentFile guards the 404 path: http.FileServer handles
// missing files itself, and we just want to prove StripPrefix lines up so
// a typo in a URL produces a 404 rather than leaking into the share-page
// routes.
func TestStatic_NonexistentFile(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/static/does-not-exist.css", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", rec.Code)
	}
}

// TestShare_FileNoPassword: a plain file share, no password, GET /{id}
// returns 200 with the filename, a "Download" affordance, and a
// human-readable size in the body. Pinning substrings (not byte matches)
// because Phase 14 will iterate on the styling.
func TestShare_FileNoPassword(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	payload := []byte("hello")
	created, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "hello.txt",
		MIMEType:         "text/plain",
		Size:             int64(len(payload)),
		Content:          bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"hello.txt", "Download", "5 B", "/d/" + created.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

// TestShare_TextNoPassword: a plain text share renders its content inside
// a <pre> block alongside a "Download as .txt" link pointing at the
// download route.
func TestShare_TextNoPassword(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	content := "a secret memo"
	created, err := svc.CreateTextShare(context.Background(), share.CreateTextOpts{
		UserID:  userID,
		Content: content,
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<pre>"+content+"</pre>") {
		t.Errorf("body missing <pre>%s</pre>; got:\n%s", content, body)
	}
	if !strings.Contains(body, "/d/"+created.ID) {
		t.Errorf("body missing download link /d/%s; got:\n%s", created.ID, body)
	}
}

// TestShare_FileWithPasswordNoCookie: when the share has a password and the
// request carries no unlock cookie, GET /{id} returns 401 and renders the
// password form pointing its action at POST /{id}.
func TestShare_FileWithPasswordNoCookie(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "secret.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
		Password:         "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Password required",
		`type="password"`,
		`name="password"`,
		`action="/` + created.ID + `"`,
		`method="post"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	// the filename must NOT leak onto the prompt — the user hasn't proven
	// knowledge of the password yet, so any metadata would be a small but
	// unnecessary disclosure.
	if strings.Contains(body, "secret.bin") {
		t.Errorf("password prompt leaked filename; body:\n%s", body)
	}
}

// TestShare_FileWithPasswordValidCookie: the same share, but the request
// carries the unlock cookie set by a successful POST. The server must
// honour it and render the file view rather than the prompt.
func TestShare_FileWithPasswordValidCookie(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "secret.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
		Password:         "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+created.ID, nil)
	req.AddCookie(&http.Cookie{Name: "yacht_share_" + created.ID, Value: "1"})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"secret.bin", "Download", "/d/" + created.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

// TestShare_Missing: GET /{id} for a share that does not exist returns 404
// and renders the shared error template — never 500, never leak the ID.
func TestShare_Missing(t *testing.T) {
	srv, _, _ := newTestServerWithShare(t)

	req := httptest.NewRequest(http.MethodGet, "/nosuchid", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Not Found") {
		t.Errorf("body missing 'Not Found' title; got:\n%s", body)
	}
	if !strings.Contains(body, "<h1>") {
		t.Errorf("body missing error.html layout; got:\n%s", body)
	}
}

// TestShare_Expired: a row that exists but whose expires_at is in the past
// must surface as 410 Gone — not 404. The distinction matters because a
// user following a known-good URL that has since expired needs a different
// message than "that was never a real share".
func TestShare_Expired(t *testing.T) {
	srv, _, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	// bypass share.Service.CreateTextShare so we can set expires_at in the
	// past deterministically — the service always computes expiry relative to
	// time.Now().
	pastExpires := time.Now().Add(-1 * time.Hour).Unix()
	createdAt := time.Now().Add(-2 * time.Hour).Unix()
	id := "expired1"
	_, err := handle.ExecContext(context.Background(), `
		INSERT INTO shares
			(id, user_id, kind, text_content, created_at, expires_at, download_count)
		VALUES (?, ?, 'text', 'stale', ?, ?, 0)
	`, id, userID, createdAt, pastExpires)
	if err != nil {
		t.Fatalf("insert expired row: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+id, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status: want 410, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Gone") && !strings.Contains(body, "expired") {
		t.Errorf("body missing expiry messaging; got:\n%s", body)
	}
}
