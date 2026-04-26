package i18n

import "testing"

// withTestBundle swaps the package bundle for the test's duration so the
// lookup tests assert against well-known fixtures instead of the real
// (still-empty in Task 2, populated in Task 5) en/ru maps. Restores the
// original bundle via t.Cleanup so the next test sees a clean state.
func withTestBundle(t *testing.T, b map[string]map[string]string) {
	t.Helper()
	orig := bundle
	bundle = b
	t.Cleanup(func() { bundle = orig })
}

func TestT_HappyPath(t *testing.T) {
	withTestBundle(t, map[string]map[string]string{
		"en": {"greeting": "Hello"},
		"ru": {"greeting": "Привет"},
	})
	if got := T("ru", "greeting"); got != "Привет" {
		t.Fatalf("T(ru, greeting) = %q, want %q", got, "Привет")
	}
	if got := T("en", "greeting"); got != "Hello" {
		t.Fatalf("T(en, greeting) = %q, want %q", got, "Hello")
	}
}

func TestT_FallbackToEnglish(t *testing.T) {
	withTestBundle(t, map[string]map[string]string{
		"en": {"only_en": "English only"},
		"ru": {},
	})
	if got := T("ru", "only_en"); got != "English only" {
		t.Fatalf("T(ru, only_en) = %q, want English fallback %q", got, "English only")
	}
}

func TestT_VisibleMiss(t *testing.T) {
	withTestBundle(t, map[string]map[string]string{
		"en": {},
		"ru": {},
	})
	if got := T("en", "missing.key"); got != "missing.key" {
		t.Fatalf("T(en, missing.key) = %q, want literal key fallback", got)
	}
	if got := T("ru", "missing.key"); got != "missing.key" {
		t.Fatalf("T(ru, missing.key) = %q, want literal key fallback", got)
	}
}

func TestT_UnsupportedLang(t *testing.T) {
	withTestBundle(t, map[string]map[string]string{
		"en": {"greeting": "Hello"},
		"ru": {"greeting": "Привет"},
	})
	// zh is outside the allowlist so step 1 is skipped — the lookup
	// must reach the English fallback rather than returning the key.
	if got := T("zh", "greeting"); got != "Hello" {
		t.Fatalf("T(zh, greeting) = %q, want English fallback %q", got, "Hello")
	}
}

func TestIsSupported(t *testing.T) {
	cases := []struct {
		lang string
		want bool
	}{
		{"en", true},
		{"ru", true},
		{"zh", false},
		{"", false},
		{"EN", false}, // case-sensitive — cookies/DB are normalized to lowercase upstream
	}
	for _, c := range cases {
		if got := IsSupported(c.lang); got != c.want {
			t.Errorf("IsSupported(%q) = %v, want %v", c.lang, got, c.want)
		}
	}
}

func TestMatchAcceptLanguage_HappyPath(t *testing.T) {
	if got := MatchAcceptLanguage("ru-RU,en;q=0.9"); got != "ru" {
		t.Fatalf("MatchAcceptLanguage(ru-RU,en;q=0.9) = %q, want %q", got, "ru")
	}
}

func TestMatchAcceptLanguage_PrefersAllowlist(t *testing.T) {
	// de is unsupported so the matcher must fall through to en (next-best
	// q-weighted match), not return de or an empty string.
	if got := MatchAcceptLanguage("de,en;q=0.9"); got != "en" {
		t.Fatalf("MatchAcceptLanguage(de,en;q=0.9) = %q, want %q", got, "en")
	}
}

func TestMatchAcceptLanguage_Empty(t *testing.T) {
	if got := MatchAcceptLanguage(""); got != "en" {
		t.Fatalf("MatchAcceptLanguage(\"\") = %q, want default %q", got, "en")
	}
}

// TestBundle_RUKeysMatchEN guards against translation drift: every key in the
// English bundle must have a counterpart in the Russian one, otherwise the
// fallback path silently surfaces English strings inside an otherwise-Russian
// page. Missing keys fail loud here long before they reach a user.
func TestBundle_RUKeysMatchEN(t *testing.T) {
	for key := range bundleEN {
		if _, ok := bundleRU[key]; !ok {
			t.Errorf("bundleRU missing key %q (present in bundleEN)", key)
		}
	}
}

// TestBundle_NoOrphanKeysInRU is the inverse of TestBundle_RUKeysMatchEN: a
// key in bundleRU with no English counterpart is almost certainly a typo
// (the renamed-EN-key, stale-RU-copy pattern). Catching it here keeps dead
// keys from accumulating in the Russian map across phases.
func TestBundle_NoOrphanKeysInRU(t *testing.T) {
	for key := range bundleRU {
		if _, ok := bundleEN[key]; !ok {
			t.Errorf("bundleRU has orphan key %q (no counterpart in bundleEN)", key)
		}
	}
}
