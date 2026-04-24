// Package auth implements the web authentication layer. Phase 9 ships the
// login side only — Telegram Login Widget (primary) and a bot-token one-time
// link (fallback), both resolving to server-side sessions stored in SQLite
// under a yacht_session cookie.
//
// Admin-only in Phase 9: only users with users.is_admin = 1 can log in. An
// otherwise-valid signature or token for a non-admin (or unknown) Telegram
// ID is rejected as ErrUnauthorized so Phase 12 can widen the allowlist to
// non-admin users with admin-managed add/remove commands without changing
// the provider call sites.
//
// The package owns its own small set of direct SQL queries against the
// sessions, login_tokens, and users tables — no ORM, no wrapper service
// layer. Every sentinel below is returned wrapped via fmt.Errorf("%w", ...),
// so callers MUST match with errors.Is and never with equality or a type
// assertion. This keeps the wrapping shape free to evolve.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
)

// AuthProvider is the common shape shared by the two login flows. Name
// identifies which provider resolved a request (persisted on the sessions
// row as the `provider` column), and Verify runs the provider's per-request
// signature / credential check plus the allowlist lookup. The returned
// *User is always an admin at this phase; non-admins come back as
// ErrUnauthorized.
//
// The bot-token flow also exposes token-mint and token-consume helpers on
// its concrete type (*BotToken) because the consumption step is stateful —
// the Verify signature on its own doesn't express "mark the row used".
// Handlers call ConsumeLoginToken directly rather than going through Verify.
type AuthProvider interface {
	Name() string
	Verify(r *http.Request) (*User, error)
}

// User is the in-memory projection of a users row, hydrated by every login
// path (widget Verify, token Consume, session Get) and consumed by the
// middleware's request context. DisplayName and Username can be empty when
// the underlying row has NULL in those columns.
type User struct {
	ID          int64
	TelegramID  int64
	Username    string
	DisplayName string
	IsAdmin     bool
}

// Error sentinels. Every return site wraps with fmt.Errorf("...: %w", sentinel)
// so callers match with errors.Is. Grouping them here keeps the surface
// discoverable and the wrap-sites consistent.
var (
	// ErrUnauthorized covers two related states: no matching users row, or
	// a matching row with is_admin = 0. Phase 9 is admin-only, so both are
	// indistinguishable from the auth flow's perspective and surface the
	// same login-page error message.
	ErrUnauthorized = errors.New("auth: unauthorized")

	// ErrInvalidSignature is widget-specific: the HMAC didn't match, or
	// the auth_date was outside the accepted freshness window.
	ErrInvalidSignature = errors.New("auth: invalid signature")

	// ErrTokenNotFound is returned by ConsumeLoginToken when no row matches
	// the supplied token string.
	ErrTokenNotFound = errors.New("auth: token not found")

	// ErrTokenExpired is returned by ConsumeLoginToken when the row exists
	// but its expires_at has already passed.
	ErrTokenExpired = errors.New("auth: token expired")

	// ErrTokenUsed is returned by ConsumeLoginToken when the row's used_at
	// column is non-NULL — the token was already redeemed.
	ErrTokenUsed = errors.New("auth: token already used")

	// ErrRateLimited is returned by CreateLoginToken when the caller has
	// already minted a token for this user within the 60-second window.
	ErrRateLimited = errors.New("auth: rate limited")

	// ErrSessionNotFound is returned by GetSession when no row matches the
	// supplied session ID.
	ErrSessionNotFound = errors.New("auth: session not found")

	// ErrSessionExpired is returned by GetSession when the row exists but
	// its expires_at has already passed.
	ErrSessionExpired = errors.New("auth: session expired")
)

// lookupUserByTelegramID resolves a Telegram user ID to an admin *User.
// Returns ErrUnauthorized when there is no matching row OR when the row
// exists but is_admin = 0 — the two cases collapse into one in Phase 9
// because both surface as "access denied" on the login page and we don't
// want login errors to double as an admin-membership oracle. Phase 12
// relaxes this to "any allowlisted user" with a different WHERE clause.
func lookupUserByTelegramID(ctx context.Context, db *sql.DB, telegramID int64) (*User, error) {
	var (
		u                          User
		username, displayName      sql.NullString
		isAdmin                    int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, telegram_id, telegram_username, display_name, is_admin
		FROM users
		WHERE telegram_id = ?
	`, telegramID).Scan(&u.ID, &u.TelegramID, &username, &displayName, &isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lookup telegram_id %d: %w", telegramID, ErrUnauthorized)
	}
	if err != nil {
		return nil, fmt.Errorf("lookup telegram_id %d: %w", telegramID, err)
	}
	if isAdmin == 0 {
		// phase 9 is admin-only: a non-admin user row is as good as absent
		// for login purposes. Phase 12 will widen this check.
		return nil, fmt.Errorf("lookup telegram_id %d: %w", telegramID, ErrUnauthorized)
	}
	u.Username = username.String
	u.DisplayName = displayName.String
	u.IsAdmin = true
	return &u, nil
}
