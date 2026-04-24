package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/db"
)

// newTestDB mirrors the helper in internal/auth's test file: a fresh
// on-disk SQLite under t.TempDir() with the full migration stack applied.
// modernc.org/sqlite's :memory: doesn't share state across pooled
// connections, so we use a temp file instead.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "middleware.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return handle
}

// insertAdminUser inserts a users row with is_admin=1 and returns its id.
// telegram_id is supplied by the caller so two users in one test don't
// collide on the UNIQUE constraint.
func insertAdminUser(t *testing.T, handle *sql.DB, telegramID int64) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 1, strftime('%s','now'))`,
		telegramID, "admin", "Admin User",
	)
	if err != nil {
		t.Fatalf("insert admin user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// cookieName is the default session cookie name used across these tests.
// Matches defaults in config so the assertions read naturally.
const cookieName = "yacht_session"

// okHandler is the downstream handler the middleware wraps in happy-path
// tests. It writes a 200 and a marker body so the test can assert the
// wrapped handler actually ran.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// findCookie returns the first Set-Cookie on the recorder whose name matches
// target, or nil if none was set. Mirrors the helper in the web package.
func findCookie(rec *httptest.ResponseRecorder, target string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == target {
			return c
		}
	}
	return nil
}

// TestRequireAuth_HappyPath: a real session cookie for an admin user lets
// the wrapped handler run and does not emit a Set-Cookie clearing header.
func TestRequireAuth_HappyPath(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertAdminUser(t, handle, 7001)
	sessionID, err := auth.CreateSession(ctx, handle, userID, "telegram_widget", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	mw := RequireAuth(handle, cookieName)
	handler := mw(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "ok" {
		t.Errorf("body: want %q, got %q", "ok", body)
	}
	// a happy-path traversal must NOT stomp the caller's cookie.
	if c := findCookie(rec, cookieName); c != nil {
		t.Errorf("unexpected Set-Cookie on happy path: %+v", c)
	}
}

// TestRequireAuth_SetsUserInContext: the wrapped handler can read the
// resolved *auth.User back via UserFromContext, and the fields match the
// admin row we inserted. This is the contract Phase 10's upload handlers
// will rely on.
func TestRequireAuth_SetsUserInContext(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertAdminUser(t, handle, 7002)
	sessionID, err := auth.CreateSession(ctx, handle, userID, "bot_token", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var gotUser *auth.User
	var gotOK bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotOK = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := RequireAuth(handle, cookieName)
	handler := mw(inner)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !gotOK {
		t.Fatal("UserFromContext ok=false; want true")
	}
	if gotUser == nil {
		t.Fatal("UserFromContext returned nil user")
	}
	if gotUser.ID != userID {
		t.Errorf("user.ID = %d, want %d", gotUser.ID, userID)
	}
	if gotUser.TelegramID != 7002 {
		t.Errorf("user.TelegramID = %d, want 7002", gotUser.TelegramID)
	}
	if !gotUser.IsAdmin {
		t.Error("user.IsAdmin = false, want true")
	}
}

// TestRequireAuth_NoCookie: a request without the session cookie is
// redirected to /login with 303 See Other and the wrapped handler is
// NOT invoked. No Set-Cookie is emitted because there's nothing to clear.
func TestRequireAuth_NoCookie(t *testing.T) {
	handle := newTestDB(t)

	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	mw := RequireAuth(handle, cookieName)
	handler := mw(inner)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("wrapped handler should NOT be called when no cookie is present")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
	// no cookie to clear → no Set-Cookie header.
	if c := findCookie(rec, cookieName); c != nil {
		t.Errorf("unexpected Set-Cookie for no-cookie case: %+v", c)
	}
}

// TestRequireAuth_EmptyCookieValue: a cookie is present but its value is
// empty. Treated the same as "no cookie" — redirect to /login, wrapped
// handler not called. Guards against a buggy client that sends
// "Cookie: yacht_session=" after a prior clear.
func TestRequireAuth_EmptyCookieValue(t *testing.T) {
	handle := newTestDB(t)

	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	mw := RequireAuth(handle, cookieName)
	handler := mw(inner)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: ""})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("wrapped handler should NOT be called for empty cookie value")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
}

// TestRequireAuth_InvalidSession: a cookie whose value doesn't map to any
// session row redirects to /login AND emits a Set-Cookie clearing the
// caller's cookie (MaxAge=-1). Clearing is essential so the client doesn't
// keep re-sending the stale cookie on every navigation.
func TestRequireAuth_InvalidSession(t *testing.T) {
	handle := newTestDB(t)

	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	mw := RequireAuth(handle, cookieName)
	handler := mw(inner)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "nonexistentsessionid"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("wrapped handler should NOT be called for invalid session")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
	c := findCookie(rec, cookieName)
	if c == nil {
		t.Fatalf("expected clearing Set-Cookie; Set-Cookie headers: %v", rec.Header().Values("Set-Cookie"))
	}
	if c.MaxAge != -1 {
		t.Errorf("cookie MaxAge: want -1, got %d", c.MaxAge)
	}
	if c.Value != "" {
		t.Errorf("cookie Value: want empty, got %q", c.Value)
	}
	if c.Path != "/" {
		t.Errorf("cookie Path: want %q, got %q", "/", c.Path)
	}
	if !c.HttpOnly {
		t.Errorf("cookie HttpOnly: want true, got false")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite: want Lax (%d), got %d", http.SameSiteLaxMode, c.SameSite)
	}
}

// TestRequireAuth_ExpiredSession: a real session row whose expires_at has
// passed is treated like any other auth failure — redirect + clearing
// cookie. Planted directly because CreateSession only mints fresh rows.
func TestRequireAuth_ExpiredSession(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertAdminUser(t, handle, 7003)

	// plant an expired session row directly so we can guarantee expires_at
	// is in the past without clock manipulation.
	sessionID := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	_, err := handle.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, provider, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, sessionID, userID, "telegram_widget", time.Now().Add(-time.Hour).Unix(), time.Now().Add(-2*time.Hour).Unix())
	if err != nil {
		t.Fatalf("insert expired session: %v", err)
	}

	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	mw := RequireAuth(handle, cookieName)
	handler := mw(inner)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("wrapped handler should NOT be called for expired session")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
	c := findCookie(rec, cookieName)
	if c == nil {
		t.Fatalf("expected clearing Set-Cookie on expired session")
	}
	if c.MaxAge != -1 {
		t.Errorf("cookie MaxAge: want -1, got %d", c.MaxAge)
	}
}

// TestRequireAuth_NonAdminSession: a session row whose joined user row has
// is_admin=0 surfaces from GetSession as ErrUnauthorized and is mapped to
// the same redirect + clear-cookie response. Phase 9 is admin-only and
// this covers the defensive read-path check GetSession performs.
func TestRequireAuth_NonAdminSession(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	// insert a non-admin user directly (insertAdminUser only does admins).
	res, err := handle.ExecContext(ctx,
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 0, strftime('%s','now'))`,
		7004, "notadmin", "Not Admin",
	)
	if err != nil {
		t.Fatalf("insert non-admin: %v", err)
	}
	userID, _ := res.LastInsertId()

	sessionID := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	_, err = handle.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, provider, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, sessionID, userID, "telegram_widget", time.Now().Add(time.Hour).Unix(), time.Now().Unix())
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	mw := RequireAuth(handle, cookieName)
	handler := mw(inner)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("wrapped handler should NOT be called for non-admin session")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	c := findCookie(rec, cookieName)
	if c == nil {
		t.Fatalf("expected clearing Set-Cookie for non-admin session")
	}
	if c.MaxAge != -1 {
		t.Errorf("cookie MaxAge: want -1, got %d", c.MaxAge)
	}
}

// TestUserFromContext_Missing: a context that never went through the
// middleware returns (nil, false) instead of panicking on a type assertion.
// This is the contract public-route handlers rely on when they optionally
// check for a logged-in user without gating on it.
func TestUserFromContext_Missing(t *testing.T) {
	u, ok := UserFromContext(context.Background())
	if ok {
		t.Error("UserFromContext ok = true on empty context; want false")
	}
	if u != nil {
		t.Errorf("UserFromContext user = %+v; want nil", u)
	}
}

// TestContextWithUser_RoundTrip: storing and retrieving a user round-trips
// cleanly — the same pointer comes back out. Guards the ContextWithUser +
// UserFromContext pair against accidental key drift.
func TestContextWithUser_RoundTrip(t *testing.T) {
	want := &auth.User{ID: 42, TelegramID: 99, IsAdmin: true}
	ctx := ContextWithUser(context.Background(), want)

	got, ok := UserFromContext(ctx)
	if !ok {
		t.Fatal("UserFromContext ok = false on context built via ContextWithUser")
	}
	if got != want {
		t.Errorf("user pointer mismatch: got %p, want %p", got, want)
	}
}
