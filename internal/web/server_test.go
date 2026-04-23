package web

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// postPasswordRequest builds a POST /{id} request with an
// application/x-www-form-urlencoded body carrying the supplied password.
// Shared between the password-flow tests so a future rewrite of the form
// shape (e.g. adding a CSRF token) only touches one place.
func postPasswordRequest(t *testing.T, id, password string) *http.Request {
	t.Helper()
	form := url.Values{}
	form.Set("password", password)
	req := httptest.NewRequest(http.MethodPost, "/"+id, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// findCookie returns the first Set-Cookie response cookie whose name matches
// target, or nil if none was set. httptest.ResponseRecorder exposes the raw
// Set-Cookie headers via Result(); http.Response.Cookies() parses them.
func findCookie(rec *httptest.ResponseRecorder, target string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == target {
			return c
		}
	}
	return nil
}

// TestPassword_Correct: a matching password sets the unlock cookie with the
// expected scope (Path=/, SameSite=Strict, HttpOnly, Max-Age=300) and 303-
// redirects the browser back to GET /{id}. The redirect target is a relative
// path so the response survives reverse-proxy rewrites.
func TestPassword_Correct(t *testing.T) {
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

	req := postPasswordRequest(t, created.ID, "hunter2")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/"+created.ID {
		t.Errorf("location: want %q, got %q", "/"+created.ID, loc)
	}
	c := findCookie(rec, "yacht_share_"+created.ID)
	if c == nil {
		t.Fatalf("unlock cookie not set; Set-Cookie headers: %v", rec.Header().Values("Set-Cookie"))
	}
	if c.Value != "1" {
		t.Errorf("cookie value: want %q, got %q", "1", c.Value)
	}
	if c.Path != "/" {
		t.Errorf("cookie path: want %q, got %q", "/", c.Path)
	}
	if !c.HttpOnly {
		t.Errorf("cookie HttpOnly: want true, got false")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite: want Strict (%d), got %d", http.SameSiteStrictMode, c.SameSite)
	}
	if c.MaxAge != 300 {
		t.Errorf("cookie MaxAge: want 300, got %d", c.MaxAge)
	}
}

// TestPassword_Incorrect: a rejected password re-renders the prompt at 401
// with the error banner populated, and crucially does NOT set an unlock
// cookie — otherwise any POST would inflate the trust token pool.
func TestPassword_Incorrect(t *testing.T) {
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

	req := postPasswordRequest(t, created.ID, "wrong")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Password required",
		"Incorrect password",
		`action="/` + created.ID + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	if c := findCookie(rec, "yacht_share_"+created.ID); c != nil {
		t.Errorf("unlock cookie should NOT be set on failure; got %+v", c)
	}
}

// TestPassword_MissingShare: POST /{id} for a share that doesn't exist
// returns 404 using the same error.html mapping as shareHandler. The user
// should never be able to tell from the response whether the share existed
// and had a different password or didn't exist at all.
func TestPassword_MissingShare(t *testing.T) {
	srv, _, _ := newTestServerWithShare(t)

	req := postPasswordRequest(t, "nosuchid", "anything")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Not Found") {
		t.Errorf("body missing 'Not Found'; got:\n%s", rec.Body.String())
	}
}

// TestPassword_ExpiredShare: POST /{id} for a share whose expires_at has
// passed returns 410 Gone — consistent with the GET mapping so the user's
// experience doesn't depend on which request the expiry happened to race.
func TestPassword_ExpiredShare(t *testing.T) {
	srv, _, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	// bypass share.Service so we can set expires_at in the past; include a
	// password_hash so we exercise the "expired then password check" order
	// rather than short-circuiting on the no-password branch first.
	pastExpires := time.Now().Add(-1 * time.Hour).Unix()
	createdAt := time.Now().Add(-2 * time.Hour).Unix()
	id := "expired2"
	hash := "$2a$10$abcdefghijklmnopqrstuv" // shape-only; handler never reaches the compare path
	_, err := handle.ExecContext(context.Background(), `
		INSERT INTO shares
			(id, user_id, kind, text_content, password_hash, created_at, expires_at, download_count)
		VALUES (?, ?, 'text', 'stale', ?, ?, ?, 0)
	`, id, userID, hash, createdAt, pastExpires)
	if err != nil {
		t.Fatalf("insert expired row: %v", err)
	}

	req := postPasswordRequest(t, id, "anything")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status: want 410, got %d; body=%q", rec.Code, rec.Body.String())
	}
}

// TestPassword_ShareWithoutPassword: POST /{id} against an unprotected share
// should never happen via the normal UX (the share page skips the prompt),
// so surface as 400 rather than silently redirecting or landing a cookie.
// Catches future regressions that might start posting to unprotected shares.
func TestPassword_ShareWithoutPassword(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "open.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := postPasswordRequest(t, created.ID, "anything")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if c := findCookie(rec, "yacht_share_"+created.ID); c != nil {
		t.Errorf("unlock cookie should NOT be set for unprotected share; got %+v", c)
	}
}

// TestDownload_File: a plain file share streams its bytes with the
// Content-Type and Content-Length from the row and an RFC-6266-compliant
// Content-Disposition that exposes the uploader's original filename. The
// download_count column goes from 0 to 1 after the handler returns, proving
// the post-stream IncrementDownloadCount ran on the detached ctx.
func TestDownload_File(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	payload := []byte("hello world")
	created, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "greeting.txt",
		MIMEType:         "text/plain",
		Size:             int64(len(payload)),
		Content:          bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/d/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("content-type: want %q, got %q", "text/plain", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "11" {
		t.Errorf("content-length: want %q, got %q", "11", cl)
	}
	cd := rec.Header().Get("Content-Disposition")
	for _, want := range []string{
		`attachment;`,
		`filename="greeting.txt"`,
		`filename*=UTF-8''greeting.txt`,
	} {
		if !strings.Contains(cd, want) {
			t.Errorf("content-disposition missing %q; got %q", want, cd)
		}
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, payload) {
		t.Errorf("body: want %q, got %q", payload, got)
	}

	// confirm IncrementDownloadCount ran via the detached ctx.
	after, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get after download: %v", err)
	}
	if after.DownloadCount != 1 {
		t.Errorf("download_count: want 1, got %d", after.DownloadCount)
	}
}

// TestDownload_Text: a text share is served as a text/plain attachment named
// {shareID}.txt. The body is the stored text content verbatim — no HTML
// wrapping, no template. download_count moves from 0 to 1 the same way.
func TestDownload_Text(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/d/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type: want %q, got %q", "text/plain; charset=utf-8", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "13" {
		t.Errorf("content-length: want %q, got %q", "13", cl)
	}
	wantCD := `attachment; filename="` + created.ID + `.txt"`
	if cd := rec.Header().Get("Content-Disposition"); cd != wantCD {
		t.Errorf("content-disposition: want %q, got %q", wantCD, cd)
	}
	if got := rec.Body.String(); got != content {
		t.Errorf("body: want %q, got %q", content, got)
	}

	after, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get after download: %v", err)
	}
	if after.DownloadCount != 1 {
		t.Errorf("download_count: want 1, got %d", after.DownloadCount)
	}
}

// TestDownload_UTF8Filename: a file whose original filename carries non-ASCII
// bytes must be exposed via the RFC 5987 filename*= form with correct
// percent-encoding of the UTF-8 bytes. The ASCII filename= fallback is
// underscored — browsers that understand filename*= ignore it, but it must
// still be a valid quoted-string per RFC 6266.
func TestDownload_UTF8Filename(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	payload := []byte("hi")
	name := "привет.pdf"
	created, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: name,
		MIMEType:         "application/pdf",
		Size:             int64(len(payload)),
		Content:          bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/d/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	// "привет.pdf" UTF-8 bytes: D0 BF D1 80 D0 B8 D0 B2 D0 B5 D1 82 2E 70 64 66
	wantExt := "filename*=UTF-8''%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82.pdf"
	if !strings.Contains(cd, wantExt) {
		t.Errorf("content-disposition missing %q; got %q", wantExt, cd)
	}
	// ASCII fallback must exist and not contain the raw non-ASCII bytes.
	if !strings.Contains(cd, `filename="`) {
		t.Errorf("content-disposition missing quoted filename= fallback; got %q", cd)
	}
	for _, b := range []byte(name) {
		if b >= 0x80 {
			if strings.IndexByte(cd, b) >= 0 {
				t.Errorf("content-disposition fallback leaks non-ASCII byte 0x%02x; got %q", b, cd)
				break
			}
		}
	}
}

// TestDownload_Missing: GET /d/{id} for a nonexistent share returns 404 via
// the same error mapping as the share page. The user should not be able to
// tell from the status whether the id ever existed.
func TestDownload_Missing(t *testing.T) {
	srv, _, _ := newTestServerWithShare(t)

	req := httptest.NewRequest(http.MethodGet, "/d/nosuchid", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Not Found") {
		t.Errorf("body missing 'Not Found'; got:\n%s", rec.Body.String())
	}
}

// TestDownload_Expired: an existing row whose expires_at has passed surfaces
// as 410 Gone on the download endpoint just like on the share page — a
// lapsed URL should never start streaming bytes.
func TestDownload_Expired(t *testing.T) {
	srv, _, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	pastExpires := time.Now().Add(-1 * time.Hour).Unix()
	createdAt := time.Now().Add(-2 * time.Hour).Unix()
	id := "expireddl"
	_, err := handle.ExecContext(context.Background(), `
		INSERT INTO shares
			(id, user_id, kind, text_content, created_at, expires_at, download_count)
		VALUES (?, ?, 'text', 'stale', ?, ?, 0)
	`, id, userID, createdAt, pastExpires)
	if err != nil {
		t.Fatalf("insert expired row: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/d/"+id, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status: want 410, got %d; body=%q", rec.Code, rec.Body.String())
	}
}

// TestDownload_WithPasswordNoCookie: a password-protected share hit without
// an unlock cookie must render the password prompt at 401 rather than
// streaming bytes. Redirecting is wrong here — the user is already on the
// /d/ URL and we'd bounce them away from the link they clicked.
func TestDownload_WithPasswordNoCookie(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/d/"+created.ID, nil)
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
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	after, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get after blocked download: %v", err)
	}
	if after.DownloadCount != 0 {
		t.Errorf("download_count: want 0 (prompt, no stream), got %d", after.DownloadCount)
	}
}

// TestDownload_WithPasswordValidCookie: the same protected share, but the
// request carries the unlock cookie set by a successful POST. The server
// must honour it and stream the bytes instead of re-prompting.
func TestDownload_WithPasswordValidCookie(t *testing.T) {
	srv, svc, handle := newTestServerWithShare(t)
	userID := insertWebTestUser(t, handle)

	payload := []byte{1, 2, 3}
	created, err := svc.CreateFileShare(context.Background(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "secret.bin",
		MIMEType:         "application/octet-stream",
		Size:             int64(len(payload)),
		Content:          bytes.NewReader(payload),
		Password:         "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/d/"+created.ID, nil)
	req.AddCookie(&http.Cookie{Name: "yacht_share_" + created.ID, Value: "1"})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, payload) {
		t.Errorf("body: want %v, got %v", payload, got)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, `filename="secret.bin"`) {
		t.Errorf("content-disposition: missing quoted filename; got %q", cd)
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
