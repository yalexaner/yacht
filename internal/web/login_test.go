package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yalexaner/yacht/internal/config"
)

// newLoginTestServer builds a Server pointed at a config with a known
// TelegramBotUsername so the login page's widget/fallback can be asserted
// on deterministically. The share service is nil — the login route never
// touches it.
func newLoginTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Web{
		Shared:              &config.Shared{},
		TelegramBotUsername: "yachtshare_bot",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, nil, nil, nil, nil, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// TestLogin_RendersWidget: GET /login returns 200 with the Telegram Login
// Widget script, the configured bot username in both the widget attributes
// and the fallback @-link, the fallback warning text, and no error banner
// on a clean first-display (no ?error= query param).
func TestLogin_RendersWidget(t *testing.T) {
	srv := newLoginTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type: want text/html prefix, got %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"telegram-widget.js",
		`data-telegram-login="yachtshare_bot"`,
		`data-auth-url="/auth/telegram/callback"`,
		"https://t.me/yachtshare_bot",
		"/weblogin",
		"widget may be blocked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, `<p class="error">`) {
		t.Errorf("error banner rendered without an error code; body:\n%s", body)
	}
}

// TestLogin_RendersErrorBanner: GET /login?error=link_expired surfaces the
// human-readable message from loginErrorMessages inside the error banner.
// Asserts the mapping wiring plus the template's conditional block.
func TestLogin_RendersErrorBanner(t *testing.T) {
	srv := newLoginTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login?error=link_expired", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<p class="error">`) {
		t.Errorf("error banner missing; body:\n%s", body)
	}
	if !strings.Contains(body, "login link has expired") {
		t.Errorf("body missing mapped error message; got:\n%s", body)
	}
}

// TestLogin_UnknownErrorCodeDropped: a ?error= value that isn't in the
// mapping table must render no banner at all. Prevents an attacker from
// echoing arbitrary text (or markup) through the query param — even though
// html/template would auto-escape it, not echoing unknown strings is the
// stricter guarantee.
func TestLogin_UnknownErrorCodeDropped(t *testing.T) {
	srv := newLoginTestServer(t)

	req := httptest.NewRequest(http.MethodGet, `/login?error=<script>alert(1)</script>`, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `<p class="error">`) {
		t.Errorf("error banner rendered for unknown code; body:\n%s", body)
	}
	// defense-in-depth: even if a future change starts echoing unknown
	// codes, html/template must never emit the raw <script> tag.
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("body leaked raw <script> from query param; got:\n%s", body)
	}
}

// TestLogin_RendersRussianWithCookie: a yacht_lang=ru cookie on a public
// route must flow through the lang middleware and into the rendered
// template, producing Russian copy in place of the English defaults. The
// login page is the convenient probe — it's public (no session plumbing
// required) and exercises a representative slice of the bundle: page
// heading, description, and the login fallback subkeys all appear in one
// render.
func TestLogin_RendersRussianWithCookie(t *testing.T) {
	srv := newLoginTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_lang", Value: "ru"})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Вход",
		"Войдите через свой аккаунт Telegram.",
		"Не видите кнопку «Войти через Telegram»?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing Russian string %q; got:\n%s", want, body)
		}
	}
	// The English heading must NOT leak through when ru is requested. Pin
	// the substring to the heading specifically (not "Log in" alone, which
	// would also match the URL "/login") so we catch a genuine fall-through
	// rather than a false-positive on the path string.
	if strings.Contains(body, ">Log in</h1>") {
		t.Errorf("body leaked English heading despite ru cookie; got:\n%s", body)
	}
	// The <html lang="..."> attribute must reflect the resolved language so
	// assistive tech and search engines see the correct locale.
	if !strings.Contains(body, `<html lang="ru">`) {
		t.Errorf(`body missing <html lang="ru">; got:`+"\n%s", body)
	}
}

// TestLogin_RendersErrorBannerInRussian: with yacht_lang=ru cookie set,
// GET /login?error=link_expired must surface the Russian translation of
// the auth-error message. Locks in Task 7's loginErrorMessages → bundle-
// key conversion: a regression that returned the raw English string (or
// the bundle key itself) would fail this assertion.
func TestLogin_RendersErrorBannerInRussian(t *testing.T) {
	srv := newLoginTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login?error=link_expired", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_lang", Value: "ru"})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<p class="error">`) {
		t.Errorf("error banner missing; body:\n%s", body)
	}
	// "Срок действия ссылки истёк" is the RU translation of the
	// link_expired auth error; assert a stable substring rather than the
	// full sentence so a tweak to the translation copy doesn't fail this
	// test for the wrong reason.
	if !strings.Contains(body, "Срок действия ссылки истёк") {
		t.Errorf("body missing Russian error message; got:\n%s", body)
	}
	// Defense-in-depth: the bundle key must never leak through as the
	// rendered text. A regression that forgot the T() lookup (or used
	// the wrong key) would surface as the literal "error.auth.link_expired"
	// in the response body.
	if strings.Contains(body, "error.auth.link_expired") {
		t.Errorf("body leaked bundle key as text; got:\n%s", body)
	}
}
