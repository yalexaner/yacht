// lang.go resolves the effective language for the request and stores it on
// context. Chain: yacht_lang cookie → users.lang from session → Accept-Language
// → default. Handlers downstream call LangFromContext to pick the resolved
// value when feeding render call sites.
//
// The middleware is intentionally untied from the auth gate so it can wrap
// public routes too (login, share view, download). When auth runs upstream
// (RequireAuth → ResolveLang) and stashes a *auth.User on the context, the
// users.lang branch fires; otherwise the chain skips straight to
// Accept-Language. The shared ctxKey type from auth.go guarantees no
// collision between userCtxKey and langCtxKey even though both are
// struct{} — Go's type identity for unnamed types is per-declaration.

package middleware

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/yalexaner/yacht/internal/i18n"
)

// langKey is a dedicated unnamed-struct key type so no other package can
// collide with the lang context value. Mirrors the userCtxKey pattern in
// auth.go; the two are different types because they're declared
// separately.
type langKey struct{}

// langCtxKey is the single context key for the resolved language string.
// LangFromContext and ContextWithLang both reference this so the round-trip
// stays in lockstep without either side re-declaring the literal.
var langCtxKey = langKey{}

// ContextWithLang returns a child context carrying lang for retrieval by
// LangFromContext. ResolveLang's middleware calls this after picking the
// effective tag.
func ContextWithLang(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, langCtxKey, lang)
}

// LangFromContext returns the language string stashed on ctx by
// ResolveLang's middleware, or "en" when nothing is set. The "en" default
// keeps handlers safe to call T(LangFromContext(ctx), ...) even on a code
// path that bypassed the middleware entirely (e.g. a unit test calling the
// handler directly without a wrapping chain).
func LangFromContext(ctx context.Context) string {
	if l, ok := ctx.Value(langCtxKey).(string); ok && l != "" {
		return l
	}
	return "en"
}

// ResolveLang returns an http.Handler middleware that picks the effective
// language for the request and stashes it on the context. cookieName is the
// language cookie name (default "yacht_lang"); defaultLang is the final
// fallback when every earlier branch misses.
//
// Resolution chain (first match wins):
//
//  1. yacht_lang cookie value, if i18n.IsSupported. Unknown values are
//     ignored — a stale or hand-edited cookie should never escape into the
//     rendered page.
//  2. The *auth.User in context (set upstream by RequireAuth) when
//     user.Lang is non-nil and i18n.IsSupported. Public routes that don't
//     run RequireAuth simply skip this branch.
//  3. Accept-Language via i18n.MatchAcceptLanguage. Empty headers collapse
//     to the matcher default ("en").
//  4. defaultLang as the absolute floor.
//
// The db parameter is reserved for future read-through behavior (e.g.
// looking up the user's lang on a public route via session cookie alone)
// and is currently unused — accepting it keeps the signature stable so
// callers don't need to change when that branch lands.
func ResolveLang(db *sql.DB, cookieName, defaultLang string) func(http.Handler) http.Handler {
	_ = db
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lang := resolveLang(r, cookieName, defaultLang)
			next.ServeHTTP(w, r.WithContext(ContextWithLang(r.Context(), lang)))
		})
	}
}

// resolveLang walks the chain documented on ResolveLang and returns the
// first match. Split out as a plain function so tests can exercise the
// chain without spinning up a full middleware + handler round-trip.
func resolveLang(r *http.Request, cookieName, defaultLang string) string {
	if c, err := r.Cookie(cookieName); err == nil && i18n.IsSupported(c.Value) {
		return c.Value
	}
	if user, ok := UserFromContext(r.Context()); ok && user != nil && user.Lang != nil && i18n.IsSupported(*user.Lang) {
		return *user.Lang
	}
	if h := r.Header.Get("Accept-Language"); h != "" {
		return i18n.MatchAcceptLanguage(h)
	}
	if i18n.IsSupported(defaultLang) {
		return defaultLang
	}
	return "en"
}
