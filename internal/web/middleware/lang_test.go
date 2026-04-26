package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yalexaner/yacht/internal/auth"
)

// langCookieName matches the default cookie name used by the lang
// middleware in production. Aliased here so the assertions read naturally.
const langCookieName = "yacht_lang"

// captureLang returns a handler that records the language LangFromContext
// returns when invoked. Tests assert against the captured value.
func captureLang(into *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*into = LangFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestResolveLang_CookieWins(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "en")
	handler := mw(captureLang(&got))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "ru"})
	req.Header.Set("Accept-Language", "en-US")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want %q", got, "ru")
	}
}

func TestResolveLang_UnknownCookieFallsThrough(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "en")
	handler := mw(captureLang(&got))

	// "zh" is outside the allowlist — the middleware must IGNORE the
	// cookie entirely and continue down the chain. Accept-Language "ru"
	// is the next candidate so we expect "ru" back, proving the cookie
	// was discarded rather than letting "zh" leak through.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "zh"})
	req.Header.Set("Accept-Language", "ru")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want fallthrough to %q", got, "ru")
	}
}

func TestResolveLang_UserLangUsedWhenNoCookie(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "en")
	handler := mw(captureLang(&got))

	ru := "ru"
	user := &auth.User{ID: 1, TelegramID: 100, IsAdmin: true, Lang: &ru}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(ContextWithUser(req.Context(), user))
	// Accept-Language disagrees so a passing test proves the user.lang
	// branch fired ahead of Accept-Language.
	req.Header.Set("Accept-Language", "en-US")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want user.Lang %q", got, "ru")
	}
}

func TestResolveLang_NilUserLangFallsThrough(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "en")
	handler := mw(captureLang(&got))

	// User row is present but Lang is NULL (never picked). Must skip the
	// users.lang branch and use Accept-Language instead.
	user := &auth.User{ID: 1, TelegramID: 100, IsAdmin: true, Lang: nil}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(ContextWithUser(req.Context(), user))
	req.Header.Set("Accept-Language", "ru-RU,en;q=0.9")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want Accept-Language %q", got, "ru")
	}
}

func TestResolveLang_AcceptLanguageWhenNothingElse(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "en")
	handler := mw(captureLang(&got))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "ru-RU,en;q=0.9")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want %q", got, "ru")
	}
}

func TestResolveLang_DefaultWhenEverythingMissing(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "ru")
	handler := mw(captureLang(&got))

	// no cookie, no user, no Accept-Language → defaultLang.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want defaultLang %q", got, "ru")
	}
}

func TestResolveLang_CookieBeatsUserLang(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "en")
	handler := mw(captureLang(&got))

	en := "en"
	user := &auth.User{ID: 1, TelegramID: 100, IsAdmin: true, Lang: &en}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(ContextWithUser(req.Context(), user))
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "ru"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want cookie %q to beat user.Lang", got, "ru")
	}
}

func TestResolveLang_UserLangBeatsAcceptLanguage(t *testing.T) {
	var got string
	mw := ResolveLang(nil, langCookieName, "en")
	handler := mw(captureLang(&got))

	ru := "ru"
	user := &auth.User{ID: 1, TelegramID: 100, IsAdmin: true, Lang: &ru}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(ContextWithUser(req.Context(), user))
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "ru" {
		t.Fatalf("lang = %q, want user.Lang %q to beat Accept-Language", got, "ru")
	}
}

func TestLangFromContext_DefaultsToEnglish(t *testing.T) {
	// A context that never traversed the middleware must yield "en" so
	// downstream T() lookups don't crash on an empty string. Mirrors the
	// safety-net contract in the function doc.
	if got := LangFromContext(context.Background()); got != "en" {
		t.Fatalf("LangFromContext(background) = %q, want %q", got, "en")
	}
}

func TestContextWithLang_RoundTrip(t *testing.T) {
	ctx := ContextWithLang(context.Background(), "ru")
	if got := LangFromContext(ctx); got != "ru" {
		t.Fatalf("round-trip lang = %q, want %q", got, "ru")
	}
}
