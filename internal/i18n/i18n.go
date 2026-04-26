// Package i18n provides translation lookup for the web and bot surfaces.
// Phase 11 ships en + ru. T(lang, key) returns the bundle entry, falls back
// to English, and finally falls back to the key string itself so a missing
// translation is loud (the literal key shows up in the UI). Plurals and
// ICU MessageFormat are deferred — bundle values use printf-style %s
// placeholders and callers do their own fmt.Sprintf at the call site.
//
// Lookup is a flat dotted-key namespace (page.login.title, bot.reply.help,
// error.share.expired, button.copy). The bundle itself is a plain
// map[lang]map[key]string declared per-language in en.go / ru.go and joined
// here. A future translator workflow can swap the maps for files without
// touching the public surface.
package i18n

import "golang.org/x/text/language"

// Languages is the exported allowlist of supported language tags. Anything
// outside this list is rejected by IsSupported and ignored by middleware /
// the /lang/{code} handler so unknown values can never escape into a
// rendered page or stored cookie.
var Languages = []string{"en", "ru"}

// matcher backs MatchAcceptLanguage. The first tag is the default the
// matcher returns when nothing in the header matches, so language.English
// MUST stay first to keep the documented "fallback to en" behavior.
var matcher = language.NewMatcher([]language.Tag{
	language.English,
	language.Russian,
})

// bundle is the per-language lookup table. The inner maps live in en.go /
// ru.go so adding a third language is one new file and one extra entry
// here. T resolves against this map.
var bundle = map[string]map[string]string{
	"en": bundleEN,
	"ru": bundleRU,
}

// IsSupported reports whether lang is in the Languages allowlist. Used by
// the /lang/{code} handler to reject unknown codes with 400 and by the
// middleware to validate cookie / DB-stored values before trusting them.
func IsSupported(lang string) bool {
	for _, l := range Languages {
		if l == lang {
			return true
		}
	}
	return false
}

// MatchAcceptLanguage parses an Accept-Language header (or the equivalent
// Telegram From.LanguageCode field) and returns the closest supported
// language tag from Languages. Empty input returns the matcher's default
// ("en"). Unsupported languages collapse to the closest supported parent
// or to the default — never to the input verbatim.
func MatchAcceptLanguage(header string) string {
	tag, _ := language.MatchStrings(matcher, header)
	base, _ := tag.Base()
	code := base.String()
	if IsSupported(code) {
		return code
	}
	// MatchStrings can return a tag whose base is outside our allowlist
	// when the header parses but no supported language fits (rare, but
	// e.g. a malformed-but-recognizable tag). Collapse to default.
	return Languages[0]
}

// T looks up key in the bundle for lang. The lookup chain is:
//
//  1. bundle[lang][key] if present
//  2. bundle["en"][key] as the documented fallback
//  3. the key string itself, so a missing translation is visible in the UI
//     instead of silently rendering an empty string
//
// Callers needing placeholders do their own fmt.Sprintf on the returned
// string — T does not format. Lang values outside Languages skip step 1
// and start at the English fallback.
func T(lang, key string) string {
	if m, ok := bundle[lang]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	if m, ok := bundle["en"]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return key
}
