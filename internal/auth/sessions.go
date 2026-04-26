package auth

// Session CRUD against the sessions table. Three exported helpers plus one
// private generator, all operating on *sql.DB directly — the auth package
// owns its own tiny SQL surface rather than going through a service wrapper
// (see the package doc in auth.go for the rationale).
//
// Session IDs are 32 bytes from crypto/rand rendered as 64 hex chars. SPEC
// asks for "32+ chars"; 64 hex comfortably satisfies that while leaving no
// ambiguity about the entropy source. Cookie transport is the only consumer.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// generateSessionID returns a 64-char hex-encoded session ID sourced from
// crypto/rand. Surfacing the error (rather than panicking) keeps the caller
// in control — a rand.Read failure on Linux is effectively unreachable, but
// callers already have to return a typed error for the DB path so there's
// nothing to gain from panicking here.
func generateSessionID() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// CreateSession mints a fresh session ID and inserts it into the sessions
// table with expires_at = now + lifetime. The caller supplies the resolved
// userID (post-allowlist-check) and provider name ("telegram_widget" or
// "bot_token"); this helper is intentionally dumb about which flow minted
// the session so future providers can plug in without changing the shape.
// The returned string is the session ID the caller will put in the cookie.
func CreateSession(
	ctx context.Context,
	db *sql.DB,
	userID int64,
	provider string,
	lifetime time.Duration,
) (string, error) {
	return createSession(ctx, db, userID, provider, lifetime)
}

// CreateSessionTx is the transactional variant of CreateSession: identical
// behaviour, but the INSERT runs against the supplied *sql.Tx so the caller
// can compose session creation with other writes (e.g. atomically claiming
// a one-time login token) under a single COMMIT. botTokenHandler uses this
// to keep "mark token used" and "insert session" all-or-nothing.
func CreateSessionTx(
	ctx context.Context,
	tx *sql.Tx,
	userID int64,
	provider string,
	lifetime time.Duration,
) (string, error) {
	return createSession(ctx, tx, userID, provider, lifetime)
}

// createSession is the shared implementation backing CreateSession and
// CreateSessionTx. Accepts the dbConn subset so either *sql.DB or *sql.Tx
// satisfies the parameter without duplicating the SQL between the two
// public entry points.
func createSession(
	ctx context.Context,
	conn dbConn,
	userID int64,
	provider string,
	lifetime time.Duration,
) (string, error) {
	id, err := generateSessionID()
	if err != nil {
		return "", err
	}

	now := time.Now().Unix()
	expires := time.Now().Add(lifetime).Unix()

	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, provider, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, userID, provider, expires, now)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// GetSession resolves a session ID to its hydrated *User. The join against
// users keeps the middleware fast-path down to a single query (no separate
// user lookup). Three failure modes surface as typed sentinels so handlers
// can distinguish them:
//   - ErrSessionNotFound — no row matches (cookie stale or forged)
//   - ErrSessionExpired — row exists but expires_at < now
//   - ErrUnauthorized — row exists, not expired, but the joined user has
//     is_admin = 0 (defense-in-depth: CreateSession shouldn't have allowed
//     this, but Phase 12 may relax the insert check and this keeps the read
//     path honest regardless).
func GetSession(ctx context.Context, db *sql.DB, sessionID string) (*User, error) {
	var (
		u                           User
		expiresAt                   int64
		username, displayName, lang sql.NullString
		isAdmin                     int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT s.user_id, s.expires_at, u.telegram_id, u.telegram_username, u.display_name, u.is_admin, u.lang
		FROM sessions s
		JOIN users u ON s.user_id = u.id
		WHERE s.id = ?
	`, sessionID).Scan(&u.ID, &expiresAt, &u.TelegramID, &username, &displayName, &isAdmin, &lang)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get session: %w", ErrSessionNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	if expiresAt < time.Now().Unix() {
		return nil, fmt.Errorf("get session: %w", ErrSessionExpired)
	}

	if isAdmin == 0 {
		// same philosophy as lookupUserByTelegramID: phase 9 is admin-only,
		// so a non-admin joined row collapses into ErrUnauthorized. Phase 12
		// will relax this.
		return nil, fmt.Errorf("get session: %w", ErrUnauthorized)
	}

	u.Username = username.String
	u.DisplayName = displayName.String
	u.IsAdmin = true
	u.Lang = nullStringToPtr(lang)
	return &u, nil
}

// DeleteSession removes a session row by ID. Idempotent: a missing row is
// not an error because logout should succeed whether or not the cookie
// still maps to a live session (the user's intent is "clear my session"
// either way).
func DeleteSession(ctx context.Context, db *sql.DB, sessionID string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}
