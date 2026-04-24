package auth

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBotToken_Name(t *testing.T) {
	b := NewBotToken(nil)
	if got := b.Name(); got != "bot_token" {
		t.Errorf("Name() = %q, want %q", got, "bot_token")
	}
}

// TestBotToken_VerifyNotSupported pins the documented behavior: the
// bot-token provider implements AuthProvider for symmetry but Verify
// is not the operational entry point and must fail loudly so a
// handler mis-wiring surfaces in tests rather than silently logging
// an unauthenticated user in.
func TestBotToken_VerifyNotSupported(t *testing.T) {
	b := NewBotToken(nil)
	user, err := b.Verify(httptest.NewRequest("GET", "/auth/anything", nil))
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if err == nil {
		t.Error("expected non-nil error, got nil")
	}
}

func TestCreateLoginToken_HappyPath(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5001, true)
	b := NewBotToken(handle)

	before := time.Now().Unix()
	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}
	after := time.Now().Unix()

	if len(token) != 64 {
		t.Errorf("token length = %d, want 64", len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Errorf("token is not valid hex: %v", err)
	}

	var (
		gotUserID  int64
		gotExpires int64
		gotCreated int64
		gotUsedAt  sql.NullInt64
	)
	if err := handle.QueryRowContext(ctx,
		`SELECT user_id, expires_at, created_at, used_at FROM login_tokens WHERE token = ?`,
		token,
	).Scan(&gotUserID, &gotExpires, &gotCreated, &gotUsedAt); err != nil {
		t.Fatalf("select login_tokens row: %v", err)
	}
	if gotUserID != userID {
		t.Errorf("user_id = %d, want %d", gotUserID, userID)
	}
	if gotUsedAt.Valid {
		t.Errorf("used_at = %d, want NULL", gotUsedAt.Int64)
	}

	ttl := int64((5 * time.Minute).Seconds())
	if gotExpires < before+ttl || gotExpires > after+ttl {
		t.Errorf("expires_at = %d, want between %d and %d", gotExpires, before+ttl, after+ttl)
	}
	if gotCreated < before || gotCreated > after {
		t.Errorf("created_at = %d, want between %d and %d", gotCreated, before, after)
	}
}

func TestCreateLoginToken_RateLimited(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5002, true)
	b := NewBotToken(handle)

	if _, err := b.CreateLoginToken(ctx, userID, 5*time.Minute); err != nil {
		t.Fatalf("first CreateLoginToken: %v", err)
	}

	// second call in the same 60-second window must be rejected.
	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if token != "" {
		t.Errorf("expected empty token, got %q", token)
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}

	// exactly one row should exist — the rate-limited call must not
	// have inserted anything.
	var count int
	if err := handle.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM login_tokens WHERE user_id = ?`, userID,
	).Scan(&count); err != nil {
		t.Fatalf("count login_tokens: %v", err)
	}
	if count != 1 {
		t.Errorf("login_tokens count = %d, want 1", count)
	}
}

func TestCreateLoginToken_RateLimitResetsAfterWindow(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5003, true)
	b := NewBotToken(handle)

	// plant a token created 120 seconds ago to simulate the rate-limit
	// window having already passed. Documents the 60-second window:
	// a token minted 2 minutes ago must not block a fresh one.
	oldToken := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO login_tokens (token, user_id, expires_at, created_at)
		 VALUES (?, ?, ?, strftime('%s','now') - 120)`,
		oldToken, userID, time.Now().Add(5*time.Minute).Unix(),
	); err != nil {
		t.Fatalf("plant old token: %v", err)
	}

	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken after window: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	var count int
	if err := handle.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM login_tokens WHERE user_id = ?`, userID,
	).Scan(&count); err != nil {
		t.Fatalf("count login_tokens: %v", err)
	}
	if count != 2 {
		t.Errorf("login_tokens count = %d, want 2", count)
	}
}

func TestConsumeLoginToken_HappyPath(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	const tgID int64 = 5004
	userID := insertTestUser(t, handle, tgID, true)
	b := NewBotToken(handle)

	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	before := time.Now().Unix()
	user, err := b.ConsumeLoginToken(ctx, token)
	if err != nil {
		t.Fatalf("ConsumeLoginToken: %v", err)
	}
	after := time.Now().Unix()

	if user.ID != userID {
		t.Errorf("user.ID = %d, want %d", user.ID, userID)
	}
	if user.TelegramID != tgID {
		t.Errorf("user.TelegramID = %d, want %d", user.TelegramID, tgID)
	}
	if user.Username != "testuser" {
		t.Errorf("user.Username = %q, want %q", user.Username, "testuser")
	}
	if user.DisplayName != "Test User" {
		t.Errorf("user.DisplayName = %q, want %q", user.DisplayName, "Test User")
	}
	if !user.IsAdmin {
		t.Error("user.IsAdmin = false, want true")
	}

	// used_at should now be set to approximately "now".
	var usedAt sql.NullInt64
	if err := handle.QueryRowContext(ctx,
		`SELECT used_at FROM login_tokens WHERE token = ?`, token,
	).Scan(&usedAt); err != nil {
		t.Fatalf("select used_at: %v", err)
	}
	if !usedAt.Valid {
		t.Fatal("used_at is NULL after consume, want non-NULL")
	}
	if usedAt.Int64 < before || usedAt.Int64 > after {
		t.Errorf("used_at = %d, want between %d and %d", usedAt.Int64, before, after)
	}
}

// TestLoginTokenExists_Found returns nil for a row that's present in
// login_tokens regardless of its used_at / expires_at state. The pre-check
// only proves the token string is known; full validation is the tx-side
// caller's job.
func TestLoginTokenExists_Found(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5101, true)
	b := NewBotToken(handle)

	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	if err := b.LoginTokenExists(ctx, token); err != nil {
		t.Errorf("LoginTokenExists for live token: want nil, got %v", err)
	}

	// flip used_at — LoginTokenExists still returns nil because it does
	// not inspect token state.
	if _, err := handle.ExecContext(ctx,
		`UPDATE login_tokens SET used_at = strftime('%s','now') WHERE token = ?`, token,
	); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	if err := b.LoginTokenExists(ctx, token); err != nil {
		t.Errorf("LoginTokenExists for used token: want nil (existence-only check), got %v", err)
	}
}

// TestLoginTokenExists_NotFound returns ErrTokenNotFound for a token string
// that has no row. Lets the web handler short-circuit invalid /auth/{token}
// probes before opening the BEGIN IMMEDIATE write transaction.
func TestLoginTokenExists_NotFound(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()
	b := NewBotToken(handle)

	err := b.LoginTokenExists(ctx, "0000000000000000000000000000000000000000000000000000000000000000")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("LoginTokenExists for unknown token: want ErrTokenNotFound, got %v", err)
	}
}

func TestConsumeLoginToken_NotFound(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()
	b := NewBotToken(handle)

	user, err := b.ConsumeLoginToken(ctx, "0000000000000000000000000000000000000000000000000000000000000000")
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expected ErrTokenNotFound, got %v", err)
	}
}

func TestConsumeLoginToken_AlreadyUsed(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5005, true)
	b := NewBotToken(handle)

	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	if _, err := b.ConsumeLoginToken(ctx, token); err != nil {
		t.Fatalf("first ConsumeLoginToken: %v", err)
	}

	// second consume must be ErrTokenUsed.
	user, err := b.ConsumeLoginToken(ctx, token)
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrTokenUsed) {
		t.Errorf("expected ErrTokenUsed, got %v", err)
	}
}

func TestConsumeLoginToken_Expired(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5006, true)

	// plant a login_tokens row with expires_at in the past — we can't
	// coerce CreateLoginToken into minting one because it uses
	// time.Now for the expiry.
	token := "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO login_tokens (token, user_id, expires_at, created_at)
		 VALUES (?, ?, ?, strftime('%s','now'))`,
		token, userID, time.Now().Add(-time.Minute).Unix(),
	); err != nil {
		t.Fatalf("plant expired token: %v", err)
	}

	b := NewBotToken(handle)
	user, err := b.ConsumeLoginToken(ctx, token)
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired, got %v", err)
	}
}

// TestConsumeLoginToken_ConcurrentSingleUse pins the atomic-claim
// contract: two goroutines racing on the same token must resolve to
// exactly one successful consume and one ErrTokenUsed, never to two
// successes. Without the conditional UPDATE in ConsumeLoginToken both
// callers could pass the "usedAt.Valid" check on their SELECT and
// both return a user, violating the single-use guarantee.
func TestConsumeLoginToken_ConcurrentSingleUse(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5008, true)
	b := NewBotToken(handle)

	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		used    int
		other   []error
		start   = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			u, err := b.ConsumeLoginToken(ctx, token)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && u != nil:
				success++
			case errors.Is(err, ErrTokenUsed):
				used++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if success != 1 {
		t.Errorf("success count = %d, want exactly 1", success)
	}
	if used != racers-1 {
		t.Errorf("ErrTokenUsed count = %d, want %d", used, racers-1)
	}
	if len(other) > 0 {
		t.Errorf("unexpected errors from %d racers: %v", len(other), other)
	}
}

func TestConsumeLoginToken_NonAdminUser(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 5007, false)

	// CreateLoginToken doesn't check admin status (Phase 12 may
	// widen), so mint via the normal path. Consumption must still
	// refuse it for is_admin = 0.
	b := NewBotToken(handle)
	token, err := b.CreateLoginToken(ctx, userID, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	user, err := b.ConsumeLoginToken(ctx, token)
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}
