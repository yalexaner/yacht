// Package middleware holds the web binary's HTTP middleware stack. Phase 9
// introduces exactly one middleware, RequireAuth: it reads the session
// cookie, resolves it via auth.GetSession, and on success stashes the
// hydrated *auth.User into the request context for downstream handlers.
//
// Defined in Phase 9 but intentionally NOT applied to any route yet — the
// only route that would benefit is /upload, which lands in Phase 10. Phase
// 10 wires it in via the Server-level accessor Server.RequireAuth so the
// live surface stays: login, auth-callback, and download endpoints remain
// open, and only upload flips behind the gate.
//
// Package layout: web imports middleware, middleware imports auth — no
// cycle. Context keys are unexported (ctxKey struct{}) so no other package
// can read or overwrite the stored *auth.User by accident; ContextWithUser
// and UserFromContext are the only sanctioned entry points.
package middleware

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/yalexaner/yacht/internal/auth"
)

// ctxKey is the unexported type used as the context key for the *auth.User
// value. Using a dedicated unnamed struct (not a string or int) guarantees
// no other package can produce a colliding key even by accident — the type
// identity is only reachable through this file's helpers.
type ctxKey struct{}

// userCtxKey is the single context key used by both ContextWithUser and
// UserFromContext. Declaring it at package scope means the two helpers stay
// in sync without either party having to re-declare a literal.
var userCtxKey = ctxKey{}

// ContextWithUser returns a child context carrying u for later retrieval by
// UserFromContext. RequireAuth calls this on the successful path; handlers
// downstream of the middleware read the value back.
func ContextWithUser(ctx context.Context, u *auth.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// UserFromContext returns the *auth.User stored in ctx by RequireAuth, or
// (nil, false) when the request never traversed the middleware. Callers
// behind RequireAuth can rely on the user being present; defensive handlers
// that may also serve public routes use the ok bool to branch.
func UserFromContext(ctx context.Context) (*auth.User, bool) {
	u, ok := ctx.Value(userCtxKey).(*auth.User)
	return u, ok
}

// RequireAuth returns an http.Handler middleware that gates the wrapped
// handler behind a valid session cookie. cookieName is the session cookie
// name from config (default "yacht_session"); passing it in as a parameter
// keeps this package free of a direct config dependency.
//
// Behaviour:
//
//   - No cookie, or an empty cookie value: 303 redirect to /login, wrapped
//     handler NOT called. No Set-Cookie emitted — there is nothing to clear.
//   - Cookie present but auth.GetSession returns ErrSessionNotFound,
//     ErrSessionExpired, or ErrUnauthorized (non-admin on the joined user
//     row): 303 redirect to /login AND emit a Set-Cookie clearing the
//     client-side cookie (MaxAge=-1). Clearing is important because a stale
//     cookie would otherwise keep hammering the GetSession lookup on every
//     request until the browser's original TTL elapses.
//   - Any other error from GetSession (i.e. a real DB fault): 500. Logging
//     is the caller's concern — this middleware stays logger-free so tests
//     don't have to plumb one in.
//   - Success: stash the *auth.User on r.Context() via ContextWithUser and
//     call next with the updated request.
//
// The returned function is the idiomatic func(http.Handler) http.Handler
// middleware shape so callers can chain it with any router or adapter.
func RequireAuth(db *sql.DB, cookieName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(cookieName)
			if err != nil || c.Value == "" {
				// no cookie at all — nothing to clear, just bounce to /login.
				// This is the common path for a first-time visitor hitting a
				// gated route directly.
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}

			user, err := auth.GetSession(r.Context(), db, c.Value)
			switch {
			case err == nil:
				// fall through to the gated handler below.
			case errors.Is(err, auth.ErrSessionNotFound),
				errors.Is(err, auth.ErrSessionExpired),
				errors.Is(err, auth.ErrUnauthorized):
				// cookie is stale/forged/non-admin — clear it in the response
				// so the client doesn't keep retrying the same lookup, then
				// redirect. The cookie attributes mirror the login handler's
				// clear shape (Path=/, HttpOnly, SameSite=Lax) so browsers
				// match this against the original Set-Cookie and actually
				// drop it. Secure is left off: the clear works whether or
				// not the original was Secure, and adding it conditionally
				// would require the r.TLS / X-Forwarded-Proto plumbing the
				// web package already owns.
				http.SetCookie(w, &http.Cookie{
					Name:     cookieName,
					Value:    "",
					Path:     "/",
					MaxAge:   -1,
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
				})
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			default:
				// a real DB / driver fault. Surface as 500 so the operator
				// notices in the logs and the user sees a clean failure
				// instead of a silent redirect loop.
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), user)))
		})
	}
}
