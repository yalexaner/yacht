package auth

// Bot-token login fallback. The /weblogin bot command mints a one-time
// token, stores it in login_tokens, and DMs the user a URL. The web
// handler for /auth/{token} hands the token to ConsumeLoginToken,
// which verifies it, marks the row used, and returns the resolved
// *User. Phase 8's cleanup worker sweeps the row eventually
// (used_at IS NOT NULL OR expires_at < now → delete).
//
// The flow is stateful — minting and consumption are two separate
// calls that mutate the table — so BotToken does not fit the
// per-request AuthProvider.Verify shape cleanly. It still satisfies
// the interface (the compile-time assertion at the bottom of this
// file keeps the symmetry honest) but Verify is not the operational
// entry point: handlers call CreateLoginToken and ConsumeLoginToken
// directly.
//
// Tokens are 32 bytes from crypto/rand rendered as 64 hex chars —
// identical to session IDs. SPEC asks for "24–32 chars"; 64 hex is
// well above that, leaving no ambiguity about the entropy source.
//
// Rate limiting: one pending token per user per 60-second window. The
// window is measured in SQL with strftime('%s','now') rather than
// Go's time.Now so the comparison is monotonic with the insert's own
// created_at (both use the same SQLite clock) and immune to Go/DB
// clock drift.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// rateLimitWindowSeconds is the per-user minimum interval between
// successful CreateLoginToken calls. 60s per plan decision #8.
const rateLimitWindowSeconds = 60

// BotToken is the AuthProvider for the bot-link fallback flow. It
// holds the DB handle; there is no HMAC key to manage — the login
// token string itself is the credential.
type BotToken struct {
	db *sql.DB
}

// NewBotToken constructs a BotToken bound to the supplied DB handle.
func NewBotToken(db *sql.DB) *BotToken {
	return &BotToken{db: db}
}

// Name identifies this provider on the session row (sessions.provider
// column). Keep in sync with the callsite in the web handler.
func (*BotToken) Name() string { return "bot_token" }

// Verify is a structural member of AuthProvider but not the
// operational entry point for this flow. The bot-token lifecycle is
// stateful — consuming the token mutates the row — so a single
// per-request Verify cannot express it. Handlers must call
// ConsumeLoginToken directly. The method exists so BotToken
// implements AuthProvider, catching accidental interface drift at
// build time via the compile-time assertion below.
func (*BotToken) Verify(*http.Request) (*User, error) {
	return nil, errors.New("auth: BotToken.Verify not supported; call ConsumeLoginToken from the handler")
}

// CreateLoginToken mints a fresh single-use login token for the
// supplied userID with a lifetime of ttl. Enforces a per-user rate
// limit (one token per 60-second window) so a spammy bot client
// can't flood the DB or the user's chat with link messages. Returns
// ErrRateLimited (wrapped) when the window is still hot; on that
// path no row is inserted.
//
// Note: CreateLoginToken deliberately does NOT check is_admin — that
// gate lives on the consumption side (and on the /weblogin bot
// handler's admin dispatcher). Phase 12 widens the allowlist and
// this split keeps the mint path stable across that change.
func (b *BotToken) CreateLoginToken(
	ctx context.Context,
	userID int64,
	ttl time.Duration,
) (string, error) {
	var recent int
	err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM login_tokens
		 WHERE user_id = ? AND created_at > strftime('%s','now') - ?`,
		userID, rateLimitWindowSeconds,
	).Scan(&recent)
	if err != nil {
		return "", fmt.Errorf("create login token: check rate limit: %w", err)
	}
	if recent > 0 {
		return "", fmt.Errorf("create login token for user %d: %w", userID, ErrRateLimited)
	}

	token, err := generateLoginToken()
	if err != nil {
		return "", err
	}

	expires := time.Now().Add(ttl).Unix()
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO login_tokens (token, user_id, expires_at, created_at)
		 VALUES (?, ?, ?, strftime('%s','now'))`,
		token, userID, expires,
	); err != nil {
		return "", fmt.Errorf("create login token: insert: %w", err)
	}
	return token, nil
}

// LoginTokenExists reports whether a row exists in login_tokens for the
// supplied token string. Returns ErrTokenNotFound when no row matches and
// nil when one does — used_at / expires_at / is_admin are NOT inspected
// here, so a returned nil only means "the token string is known", not
// "redemption will succeed". The web handler calls this before opening the
// write transaction in botTokenHandler so random invalid-token probes on
// the unauthenticated /auth/{token} endpoint don't reserve the SQLite
// writer slot just to discover the row is absent. Real validation runs
// inside the tx via ConsumeLoginTokenTx, which is also where the
// concurrent-redemption guard lives.
func (b *BotToken) LoginTokenExists(ctx context.Context, token string) error {
	var dummy int
	err := b.db.QueryRowContext(ctx,
		`SELECT 1 FROM login_tokens WHERE token = ?`, token,
	).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("login token exists: %w", ErrTokenNotFound)
	}
	if err != nil {
		return fmt.Errorf("login token exists: %w", err)
	}
	return nil
}

// ConsumeLoginToken resolves a login token string to a *User, marking
// the row used_at = now as a side-effect on success. Failure modes:
//
//   - ErrTokenNotFound: no row matches (typo, expired-and-reaped, or
//     forged).
//   - ErrTokenUsed: row exists, used_at is non-NULL. A redeemed token
//     is dead for good — replay attempts return this sentinel even
//     before the cleanup worker deletes the row.
//   - ErrTokenExpired: row exists, unused, but expires_at < now. The
//     user sat on the link too long.
//   - ErrUnauthorized: row + user exist, but the joined user's
//     is_admin = 0. Phase 9 is admin-only; phase 12 relaxes.
//
// Check ordering is: used → expired → admin. "Used" beats "expired"
// because a redeemed token whose TTL has also lapsed is still more
// usefully described as "already used" — the user saw a success
// page once and may be confused why the second click fails.
//
// Concurrent redemption of the same token is handled by a conditional
// UPDATE — the "mark used" write only flips a row where used_at IS NULL,
// and the caller whose RowsAffected comes back zero loses the race and
// gets ErrTokenUsed. Without this guard, two in-flight requests with the
// same token could both pass the usedAt.Valid check and both succeed,
// defeating the single-use contract.
func (b *BotToken) ConsumeLoginToken(ctx context.Context, token string) (*User, error) {
	return consumeLoginToken(ctx, b.db, token)
}

// ConsumeLoginTokenTx is the transactional variant of ConsumeLoginToken:
// identical validation and "atomic claim" semantics, but every read and
// write runs against the supplied *sql.Tx so the caller can roll the
// used_at flip back if a downstream write (e.g. session creation) fails.
// Without this, ConsumeLoginToken would commit the mark-used immediately
// and a session-INSERT failure on the same handler would permanently burn
// the link.
func (b *BotToken) ConsumeLoginTokenTx(ctx context.Context, tx *sql.Tx, token string) (*User, error) {
	return consumeLoginToken(ctx, tx, token)
}

// consumeLoginToken is the shared implementation backing ConsumeLoginToken
// and ConsumeLoginTokenTx. Accepts the dbConn subset so either *sql.DB or
// *sql.Tx satisfies the parameter without duplicating the SQL.
func consumeLoginToken(ctx context.Context, conn dbConn, token string) (*User, error) {
	var (
		u                           User
		usedAt                      sql.NullInt64
		expiresAt                   int64
		username, displayName, lang sql.NullString
		isAdmin                     int64
	)
	// single JOIN mirrors GetSession: one round-trip for both the
	// token metadata and the user row. Keeps the error branches
	// linear and the SQL surface small.
	err := conn.QueryRowContext(ctx, `
		SELECT lt.user_id, lt.used_at, lt.expires_at,
		       u.telegram_id, u.telegram_username, u.display_name, u.is_admin, u.lang
		FROM login_tokens lt
		JOIN users u ON lt.user_id = u.id
		WHERE lt.token = ?
	`, token).Scan(&u.ID, &usedAt, &expiresAt, &u.TelegramID, &username, &displayName, &isAdmin, &lang)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("consume login token: %w", ErrTokenNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("consume login token: %w", err)
	}

	if usedAt.Valid {
		return nil, fmt.Errorf("consume login token: %w", ErrTokenUsed)
	}
	if expiresAt < time.Now().Unix() {
		return nil, fmt.Errorf("consume login token: %w", ErrTokenExpired)
	}
	if isAdmin == 0 {
		// same philosophy as lookupUserByTelegramID and GetSession:
		// phase 9 is admin-only. Phase 12 widens.
		return nil, fmt.Errorf("consume login token: %w", ErrUnauthorized)
	}

	// atomic claim: the `used_at IS NULL` guard ensures only one concurrent
	// consumer can flip the row. Any second caller that raced past the
	// SELECT-then-check above sees RowsAffected == 0 here and is folded
	// into the ErrTokenUsed branch.
	res, err := conn.ExecContext(ctx,
		`UPDATE login_tokens SET used_at = strftime('%s','now')
		 WHERE token = ? AND used_at IS NULL`,
		token,
	)
	if err != nil {
		return nil, fmt.Errorf("consume login token: mark used: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("consume login token: rows affected: %w", err)
	}
	if affected == 0 {
		return nil, fmt.Errorf("consume login token: %w", ErrTokenUsed)
	}

	u.Username = username.String
	u.DisplayName = displayName.String
	u.IsAdmin = true
	u.Lang = nullStringToPtr(lang)
	return &u, nil
}

// generateLoginToken returns a 64-char hex-encoded one-time token.
// Same shape as generateSessionID (32 bytes crypto/rand). Kept
// distinct so the intent is clear at callsites and so future
// divergence (e.g. shorter chat-friendly tokens) doesn't need to
// touch the session helper.
func generateLoginToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate login token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// compile-time assertion: BotToken satisfies AuthProvider. Structural
// only — see the note on Verify.
var _ AuthProvider = (*BotToken)(nil)
