package auth

// Telegram Login Widget verifier. Implements AuthProvider for the primary
// login flow: the widget posts the signed user fields back to our callback as
// GET query params, we re-derive the HMAC, constant-time compare it to the
// supplied hash, and (on success) resolve the Telegram ID to an admin User.
//
// Algorithm per https://core.telegram.org/widgets/login#checking-authorization:
//  1. secret_key = SHA-256(bot_token) — the raw token is NOT the key, it is
//     first hashed so compromise of the key material doesn't directly leak
//     the bot token. The 32-byte digest is the HMAC key verbatim.
//  2. data_check_string = every non-"hash" widget field rendered as
//     "key=value" lines joined with "\n", with keys sorted alphabetically.
//     Sort is REQUIRED — skipping it yields a different string and the
//     compare fails. Only fields that were actually sent participate; an
//     omitted last_name (for users without one) is NOT included as empty.
//  3. hmac_hex = hex(HMAC-SHA256(secret_key, data_check_string)).
//  4. constant-time compare hmac_hex to the supplied "hash" param — a regular
//     string compare leaks timing information about a forged signature's
//     prefix and is the textbook reason subtle.ConstantTimeCompare exists.
//  5. reject if auth_date is more than 86400 seconds (1 day) old. Telegram's
//     docs recommend this freshness window; stale auth_date collapses into
//     ErrInvalidSignature because, from the login page's perspective, the
//     two failures surface as the same "try again" outcome.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// widgetAuthDateMaxAge is the freshness window for Telegram's auth_date
// stamp. One day matches Telegram's own recommendation; anything older is
// treated as a replay attempt or a user sitting on the login page for too
// long and counts as ErrInvalidSignature.
const widgetAuthDateMaxAge = 24 * time.Hour

// widgetFieldsAllowList is the closed set of query-param names Telegram's
// widget sends. We build the data_check_string from whichever subset is
// present in a given request, and we ignore any other params a caller
// appended — an attacker can't slip extra fields into the signed payload by
// tacking them onto the URL. The list is declared sorted so the lookup is
// trivial; the sort step below is just defense-in-depth against future edits.
var widgetFieldsAllowList = []string{
	"auth_date",
	"first_name",
	"id",
	"last_name",
	"photo_url",
	"username",
}

// TelegramWidget is the AuthProvider for the Telegram Login Widget. The bot
// token it holds is the raw token from BotFather (not the SHA-256 digest) —
// hashing happens per-Verify so the token stored in config matches what the
// operator pasted.
type TelegramWidget struct {
	db       *sql.DB
	botToken string
}

// NewTelegramWidget constructs a TelegramWidget bound to the supplied DB
// handle and bot token. The token is used only as HMAC key material; this
// struct never makes outbound HTTP calls to Telegram.
func NewTelegramWidget(db *sql.DB, botToken string) *TelegramWidget {
	return &TelegramWidget{db: db, botToken: botToken}
}

// Name identifies this provider on the session row (sessions.provider
// column). Keep it in sync with the callsite in the web handler.
func (*TelegramWidget) Name() string { return "telegram_widget" }

// Verify re-derives the widget HMAC from the request's query params,
// compares it to the supplied hash in constant time, and — on success —
// resolves the embedded Telegram ID to an admin *User via the shared
// lookupUserByTelegramID helper. Typed failures:
//   - ErrInvalidSignature: HMAC mismatch, missing hash/id, malformed id,
//     or stale auth_date. All HMAC-layer failures collapse into this one
//     sentinel so the login page shows a single "signature invalid" error
//     regardless of which field was wrong.
//   - ErrUnauthorized: signature OK but the Telegram ID has no admin row.
func (w *TelegramWidget) Verify(r *http.Request) (*User, error) {
	query := r.URL.Query()

	suppliedHash := query.Get("hash")
	if suppliedHash == "" {
		return nil, fmt.Errorf("telegram widget verify: missing hash: %w", ErrInvalidSignature)
	}

	rawID := query.Get("id")
	if rawID == "" {
		return nil, fmt.Errorf("telegram widget verify: missing id: %w", ErrInvalidSignature)
	}
	telegramID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("telegram widget verify: malformed id %q: %w", rawID, ErrInvalidSignature)
	}

	rawAuthDate := query.Get("auth_date")
	if rawAuthDate == "" {
		return nil, fmt.Errorf("telegram widget verify: missing auth_date: %w", ErrInvalidSignature)
	}
	authDate, err := strconv.ParseInt(rawAuthDate, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("telegram widget verify: malformed auth_date %q: %w", rawAuthDate, ErrInvalidSignature)
	}
	// the widget's signature is time-sensitive: a valid HMAC from a week ago
	// still verifies cryptographically, but we want to reject it so a leaked
	// callback URL stops working quickly.
	age := time.Since(time.Unix(authDate, 0))
	if age > widgetAuthDateMaxAge {
		return nil, fmt.Errorf("telegram widget verify: auth_date %ds old exceeds max %ds: %w",
			int64(age.Seconds()), int64(widgetAuthDateMaxAge.Seconds()), ErrInvalidSignature)
	}

	dataCheckString := buildWidgetCheckString(query)

	// sha256(bot_token) is the HMAC key per the Telegram spec — the raw bot
	// token is NEVER used as the key directly. sha256.Sum256 returns a
	// [32]byte so we slice it for hmac.New.
	secretKey := sha256.Sum256([]byte(w.botToken))
	mac := hmac.New(sha256.New, secretKey[:])
	mac.Write([]byte(dataCheckString))
	expectedHash := hex.EncodeToString(mac.Sum(nil))

	// subtle.ConstantTimeCompare protects against timing-oracle attacks on
	// the hex-encoded hash comparison. Different lengths always return 0, so
	// the length check is implicit.
	if subtle.ConstantTimeCompare([]byte(expectedHash), []byte(suppliedHash)) != 1 {
		return nil, fmt.Errorf("telegram widget verify: hash mismatch: %w", ErrInvalidSignature)
	}

	return lookupUserByTelegramID(r.Context(), w.db, telegramID)
}

// buildWidgetCheckString renders the Telegram widget's data_check_string
// exactly as the spec prescribes: each non-"hash" field the widget sent as
// "key=value", lines joined with "\n", keys sorted alphabetically. Only
// fields that both appear in the request AND are on the widget allow list
// participate — anything else silently drops out of the signed payload, so
// an attacker can't graft arbitrary params onto the URL and have them
// folded into the HMAC.
func buildWidgetCheckString(query map[string][]string) string {
	keys := make([]string, 0, len(widgetFieldsAllowList))
	for _, k := range widgetFieldsAllowList {
		if _, ok := query[k]; ok {
			keys = append(keys, k)
		}
	}
	// belt-and-braces: allow list is declared sorted already, but re-sort
	// so future edits to the slice don't silently break HMAC verification.
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		// url.Values.Get returns the first value; matches what the widget
		// sends (single-valued params).
		b.WriteString(query[k][0])
	}
	return b.String()
}

// compile-time assertion: TelegramWidget satisfies AuthProvider. Catches
// interface drift at build time rather than at the first Verify() call.
var _ AuthProvider = (*TelegramWidget)(nil)
