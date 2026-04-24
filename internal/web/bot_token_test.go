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
	"sync"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
)

// newBotTokenTestServer builds a Server with a real *sql.DB (post-migrations)
// and a live auth.BotToken bound to that handle. SessionCookieName and
// SessionLifetime mirror production defaults so the Set-Cookie assertions
// match what a deployed binary would emit. The share service and widget
// provider are nil — the bot-token route never touches them.
func newBotTokenTestServer(t *testing.T) (*Server, *sql.DB, *auth.BotToken) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "web-bot-token.db")
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
	botToken := auth.NewBotToken(handle)

	srv, err := New(cfg, handle, nil, nil, botToken, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, handle, botToken
}

// insertBotTokenTestUser inserts a users row with the requested is_admin
// flag and returns its primary key. telegram_id uses wall-clock nanos so
// multiple users inside one test don't collide on the UNIQUE index.
func insertBotTokenTestUser(t *testing.T, handle *sql.DB, isAdmin bool) int64 {
	t.Helper()
	adminFlag := 0
	if isAdmin {
		adminFlag = 1
	}
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, ?, strftime('%s','now'))`,
		time.Now().UnixNano(), "bot-token-user", "Bot Token User", adminFlag,
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

// TestBotTokenHandler_HappyPath: a freshly-minted admin token consumed by
// the handler lands the browser at "/" with 303 See Other, sets the
// yacht_session cookie with production attributes, persists a sessions row
// tagged provider="bot_token", and flips the login_tokens row's used_at so
// a replay is blocked.
func TestBotTokenHandler_HappyPath(t *testing.T) {
	srv, handle, bot := newBotTokenTestServer(t)
	userID := insertBotTokenTestUser(t, handle, true)

	token, err := bot.CreateLoginToken(context.Background(), userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/"+token, nil)
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
		t.Error("cookie Secure: want false for plain HTTP, got true")
	}
	if wantMax := int((30 * 24 * time.Hour).Seconds()); c.MaxAge != wantMax {
		t.Errorf("cookie MaxAge: want %d (30d), got %d", wantMax, c.MaxAge)
	}
	if len(c.Value) != 64 {
		t.Errorf("cookie value length: want 64 (hex session id), got %d (%q)", len(c.Value), c.Value)
	}

	var (
		rowUserID   int64
		rowProvider string
		rowExpires  int64
	)
	err = handle.QueryRowContext(context.Background(),
		`SELECT user_id, provider, expires_at FROM sessions WHERE id = ?`, c.Value,
	).Scan(&rowUserID, &rowProvider, &rowExpires)
	if err != nil {
		t.Fatalf("lookup session row: %v", err)
	}
	if rowUserID != userID {
		t.Errorf("session user_id: want %d, got %d", userID, rowUserID)
	}
	if rowProvider != "bot_token" {
		t.Errorf("session provider: want %q, got %q", "bot_token", rowProvider)
	}
	if rowExpires <= time.Now().Unix() {
		t.Errorf("session expires_at: want future, got %d (now=%d)", rowExpires, time.Now().Unix())
	}

	var usedAt sql.NullInt64
	if err := handle.QueryRowContext(context.Background(),
		`SELECT used_at FROM login_tokens WHERE token = ?`, token,
	).Scan(&usedAt); err != nil {
		t.Fatalf("lookup login_tokens: %v", err)
	}
	if !usedAt.Valid {
		t.Error("login_tokens.used_at: want non-NULL after consume, got NULL")
	}
}

// TestBotTokenHandler_HappyPathSecureOverTLS: the session cookie must carry
// the Secure flag whenever the request arrived over TLS (direct or via the
// reverse-proxy X-Forwarded-Proto header). Mirrors the widget-callback
// coverage — a leaked session cookie on plain HTTP is a full account
// takeover, so both entry points need the same regression guard.
func TestBotTokenHandler_HappyPathSecureOverTLS(t *testing.T) {
	srv, handle, bot := newBotTokenTestServer(t)
	userID := insertBotTokenTestUser(t, handle, true)

	token, err := bot.CreateLoginToken(context.Background(), userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/"+token, nil)
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

// TestBotTokenHandler_Expired: an expired token redirects to
// /login?error=link_expired and leaves the sessions table untouched. The
// user sees a distinct message from "invalid link" so they know to request
// a fresh one rather than suspecting a typo.
func TestBotTokenHandler_Expired(t *testing.T) {
	srv, handle, _ := newBotTokenTestServer(t)
	userID := insertBotTokenTestUser(t, handle, true)

	// insert the row directly with expires_at in the past — bot.CreateLoginToken
	// won't let us produce one with a negative TTL, so bypass it for this case.
	token := "expired-" + strings.Repeat("a", 56) // 64 chars total
	pastExpires := time.Now().Add(-1 * time.Minute).Unix()
	if _, err := handle.ExecContext(context.Background(),
		`INSERT INTO login_tokens (token, user_id, expires_at, created_at)
		 VALUES (?, ?, ?, strftime('%s','now'))`,
		token, userID, pastExpires,
	); err != nil {
		t.Fatalf("insert expired token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/"+token, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=link_expired" {
		t.Errorf("location: want %q, got %q", "/login?error=link_expired", loc)
	}
	if c := findCookie(rec, "yacht_session"); c != nil {
		t.Errorf("session cookie must NOT be set on expired token; got %+v", c)
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

// TestBotTokenHandler_AlreadyUsed: a token whose used_at is already set
// redirects to /login?error=link_used. The second click on a successful
// login link is the expected failure mode this branch covers — the user
// already landed on "/" once and a distinct message helps them realize
// they don't need to reuse the link.
func TestBotTokenHandler_AlreadyUsed(t *testing.T) {
	srv, handle, bot := newBotTokenTestServer(t)
	userID := insertBotTokenTestUser(t, handle, true)

	token, err := bot.CreateLoginToken(context.Background(), userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}
	if _, err := handle.ExecContext(context.Background(),
		`UPDATE login_tokens SET used_at = strftime('%s','now') WHERE token = ?`, token,
	); err != nil {
		t.Fatalf("mark used: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/"+token, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=link_used" {
		t.Errorf("location: want %q, got %q", "/login?error=link_used", loc)
	}
	if c := findCookie(rec, "yacht_session"); c != nil {
		t.Errorf("session cookie must NOT be set on used token; got %+v", c)
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

// TestBotTokenHandler_NotFound: a token string with no matching row —
// whether a typo or a forgery attempt — redirects to /login?error=invalid_link
// without leaking the existence of any user.
func TestBotTokenHandler_NotFound(t *testing.T) {
	srv, handle, _ := newBotTokenTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/auth/"+strings.Repeat("z", 64), nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=invalid_link" {
		t.Errorf("location: want %q, got %q", "/login?error=invalid_link", loc)
	}
	if c := findCookie(rec, "yacht_session"); c != nil {
		t.Errorf("session cookie must NOT be set on unknown token; got %+v", c)
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

// TestBotTokenHandler_SessionCreateFailureKeepsTokenUnused: a transient
// failure during session creation must roll back the token's used_at flip
// so the user can retry the same link instead of being stuck behind the
// 60-second /weblogin rate limit. Drops the sessions table mid-request to
// force CreateSessionTx to fail; the test then asserts the handler returns
// 500 AND the login_tokens row stays in its pre-consume state (used_at
// IS NULL). Without the wrapping transaction, the consume would have
// committed first and the token would be permanently burned.
func TestBotTokenHandler_SessionCreateFailureKeepsTokenUnused(t *testing.T) {
	srv, handle, bot := newBotTokenTestServer(t)
	userID := insertBotTokenTestUser(t, handle, true)

	token, err := bot.CreateLoginToken(context.Background(), userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	// drop sessions so the INSERT inside CreateSessionTx returns an error,
	// exercising the rollback path. The login_tokens read still works
	// because that table is untouched.
	if _, err := handle.ExecContext(context.Background(), `DROP TABLE sessions`); err != nil {
		t.Fatalf("drop sessions: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/"+token, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 (session insert failed), got %d; body=%q", rec.Code, rec.Body.String())
	}
	if c := findCookie(rec, "yacht_session"); c != nil {
		t.Errorf("session cookie must NOT be set when session insert failed; got %+v", c)
	}

	var usedAt sql.NullInt64
	if err := handle.QueryRowContext(context.Background(),
		`SELECT used_at FROM login_tokens WHERE token = ?`, token,
	).Scan(&usedAt); err != nil {
		t.Fatalf("lookup login_tokens: %v", err)
	}
	if usedAt.Valid {
		t.Errorf("login_tokens.used_at = %v, want NULL after rollback (token must remain redeemable)", usedAt.Int64)
	}
}

// TestBotTokenHandler_ConcurrentRedemption pins the contract that two
// browsers racing on the same login link resolve to exactly one 303 → "/"
// (the winner) and N-1 303 → "/login?error=link_used" (the losers). The
// transactional consume in botTokenHandler runs read-then-update inside
// one tx, so without _txlock=immediate in the DSN the loser's UPDATE could
// surface SQLITE_BUSY_SNAPSHOT — which the handler's default branch maps
// to 500 instead of the link_used redirect, breaking the user-visible
// contract the non-transactional consume already preserves (see
// auth.TestConsumeLoginToken_ConcurrentSingleUse).
func TestBotTokenHandler_ConcurrentRedemption(t *testing.T) {
	srv, handle, bot := newBotTokenTestServer(t)
	userID := insertBotTokenTestUser(t, handle, true)

	token, err := bot.CreateLoginToken(context.Background(), userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		success  int
		linkUsed int
		other    []int
		start    = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/auth/"+token, nil)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)

			mu.Lock()
			defer mu.Unlock()
			loc := rec.Header().Get("Location")
			switch {
			case rec.Code == http.StatusSeeOther && loc == "/":
				success++
			case rec.Code == http.StatusSeeOther && loc == "/login?error=link_used":
				linkUsed++
			default:
				other = append(other, rec.Code)
			}
		}()
	}
	close(start)
	wg.Wait()

	if success != 1 {
		t.Errorf("success count = %d, want exactly 1", success)
	}
	if linkUsed != racers-1 {
		t.Errorf("link_used count = %d, want %d", linkUsed, racers-1)
	}
	if len(other) > 0 {
		t.Errorf("unexpected statuses from %d racers: %v", len(other), other)
	}

	var sessionCount int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions`,
	).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("session row count = %d, want exactly 1 (only the winner mints a session)", sessionCount)
	}
}

// TestBotTokenHandler_AccessDenied: a valid token issued for a user whose
// is_admin=0 redirects to /login?error=access_denied — Phase 9's admin-only
// gate fires on the consumption side, not just at mint time. Phase 12
// widens the allowlist and this branch will need to change alongside
// ConsumeLoginToken.
func TestBotTokenHandler_AccessDenied(t *testing.T) {
	srv, handle, bot := newBotTokenTestServer(t)
	userID := insertBotTokenTestUser(t, handle, false)

	token, err := bot.CreateLoginToken(context.Background(), userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/"+token, nil)
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
}
