package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBase_RendersSwitcher_EN: rendering any page with the resolved language
// "en" produces the switcher with English as the active label (rendered as a
// <span>) and Russian as a clickable link to /lang/ru. The login page is the
// convenient probe — public, no session plumbing needed, and it inherits the
// shared base layout that owns the switcher.
func TestBase_RendersSwitcher_EN(t *testing.T) {
	srv := newLoginTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_lang", Value: "en"})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="lang-switcher"`,
		`<span class="lang-active">English</span>`,
		`<a href="/lang/ru" hreflang="ru" class="lang-inactive">Русский</a>`,
		`<span class="lang-sep">|</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	// Defense-in-depth: the inactive language must NOT also be rendered as
	// the active <span> — that would mean the conditional collapsed to "both
	// active" and clicking the switcher would render an inert pair.
	if strings.Contains(body, `<span class="lang-active">Русский</span>`) {
		t.Errorf("Russian rendered as active despite Lang=en; got:\n%s", body)
	}
}

// TestBase_RendersSwitcher_RU: with Lang resolved to "ru", Russian is the
// active <span> and English is the link. Mirror of the EN test — exercises
// the other arm of the switcher's conditional so a one-off typo in either
// branch can't pass the suite.
func TestBase_RendersSwitcher_RU(t *testing.T) {
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
		`class="lang-switcher"`,
		`<span class="lang-active">Русский</span>`,
		`<a href="/lang/en" hreflang="en" class="lang-inactive">English</a>`,
		`<span class="lang-sep">|</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, `<span class="lang-active">English</span>`) {
		t.Errorf("English rendered as active despite Lang=ru; got:\n%s", body)
	}
}

// TestBase_HasHreflangLinks: both <link rel="alternate" hreflang="..."> tags
// must appear in <head> on every page so search engines can discover the
// language alternates. MVP uses "/" for both targets — per-page hreflang is
// Phase 14 polish.
func TestBase_HasHreflangLinks(t *testing.T) {
	srv := newLoginTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<link rel="alternate" hreflang="en" href="/">`,
		`<link rel="alternate" hreflang="ru" href="/">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}
