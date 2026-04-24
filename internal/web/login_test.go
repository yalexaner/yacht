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
