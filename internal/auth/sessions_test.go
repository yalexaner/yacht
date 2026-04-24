package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestGenerateSessionID(t *testing.T) {
	// two consecutive IDs should be valid hex of the expected length and
	// (overwhelmingly likely) distinct. The distinctness assertion doubles
	// as a smoke test that we aren't accidentally seeding with a fixed
	// source.
	id1, err := generateSessionID()
	if err != nil {
		t.Fatalf("generateSessionID: %v", err)
	}
	id2, err := generateSessionID()
	if err != nil {
		t.Fatalf("generateSessionID: %v", err)
	}

	if len(id1) != 64 {
		t.Errorf("id length = %d, want 64", len(id1))
	}
	if _, err := hex.DecodeString(id1); err != nil {
		t.Errorf("id is not valid hex: %v", err)
	}
	if id1 == id2 {
		t.Error("two consecutive IDs collided")
	}
}

func TestCreateSession_HappyPath(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 2001, true)

	lifetime := 30 * 24 * time.Hour
	before := time.Now().Unix()
	id, err := CreateSession(ctx, handle, userID, "telegram_widget", lifetime)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	after := time.Now().Unix()

	if len(id) != 64 {
		t.Errorf("id length = %d, want 64", len(id))
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("id is not valid hex: %v", err)
	}

	var (
		gotUserID   int64
		gotProvider string
		gotExpires  int64
		gotCreated  int64
	)
	err = handle.QueryRowContext(ctx,
		`SELECT user_id, provider, expires_at, created_at FROM sessions WHERE id = ?`, id,
	).Scan(&gotUserID, &gotProvider, &gotExpires, &gotCreated)
	if err != nil {
		t.Fatalf("select session row: %v", err)
	}

	if gotUserID != userID {
		t.Errorf("user_id = %d, want %d", gotUserID, userID)
	}
	if gotProvider != "telegram_widget" {
		t.Errorf("provider = %q, want %q", gotProvider, "telegram_widget")
	}
	// expires_at should be within [before+lifetime, after+lifetime].
	minExpires := before + int64(lifetime.Seconds())
	maxExpires := after + int64(lifetime.Seconds())
	if gotExpires < minExpires || gotExpires > maxExpires {
		t.Errorf("expires_at = %d, want between %d and %d", gotExpires, minExpires, maxExpires)
	}
	if gotCreated < before || gotCreated > after {
		t.Errorf("created_at = %d, want between %d and %d", gotCreated, before, after)
	}
}

func TestGetSession_HappyPath(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 2002, true)
	id, err := CreateSession(ctx, handle, userID, "bot_token", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := GetSession(ctx, handle, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != userID {
		t.Errorf("ID = %d, want %d", got.ID, userID)
	}
	if got.TelegramID != 2002 {
		t.Errorf("TelegramID = %d, want 2002", got.TelegramID)
	}
	if got.Username != "testuser" {
		t.Errorf("Username = %q, want %q", got.Username, "testuser")
	}
	if got.DisplayName != "Test User" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Test User")
	}
	if !got.IsAdmin {
		t.Error("IsAdmin = false, want true")
	}
}

func TestGetSession_NotFound(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	got, err := GetSession(ctx, handle, "0000000000000000000000000000000000000000000000000000000000000000")
	if got != nil {
		t.Errorf("expected nil user, got %+v", got)
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestGetSession_Expired(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 2003, true)

	// bypass CreateSession to plant a session with an already-past expiry —
	// CreateSession itself is time.Now-based so we can't coerce it into
	// minting an expired row without clock manipulation.
	id := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	_, err := handle.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, provider, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, userID, "telegram_widget", time.Now().Add(-time.Hour).Unix(), time.Now().Unix())
	if err != nil {
		t.Fatalf("insert expired session: %v", err)
	}

	got, err := GetSession(ctx, handle, id)
	if got != nil {
		t.Errorf("expected nil user, got %+v", got)
	}
	if !errors.Is(err, ErrSessionExpired) {
		t.Errorf("expected ErrSessionExpired, got %v", err)
	}
}

func TestGetSession_NonAdmin(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 2004, false)

	// plant a session directly to simulate a scenario CreateSession wouldn't
	// normally allow (defensive read-path check).
	id := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	_, err := handle.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, provider, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, userID, "telegram_widget", time.Now().Add(time.Hour).Unix(), time.Now().Unix())
	if err != nil {
		t.Fatalf("insert non-admin session: %v", err)
	}

	got, err := GetSession(ctx, handle, id)
	if got != nil {
		t.Errorf("expected nil user, got %+v", got)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestDeleteSession_HappyPath(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	userID := insertTestUser(t, handle, 2005, true)
	id, err := CreateSession(ctx, handle, userID, "telegram_widget", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := DeleteSession(ctx, handle, id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	got, err := GetSession(ctx, handle, id)
	if got != nil {
		t.Errorf("expected nil user after delete, got %+v", got)
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound after delete, got %v", err)
	}
}

func TestDeleteSession_Missing(t *testing.T) {
	handle := newTestDB(t)
	ctx := context.Background()

	// deleting a non-existent session is idempotent: no error, no panic.
	err := DeleteSession(ctx, handle, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}
