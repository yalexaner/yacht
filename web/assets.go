// Package web exposes the embedded HTML templates and static assets used by
// the yacht web binary's HTTP layer (internal/web). The embed directives live
// here rather than inside internal/web because Go embed patterns cannot
// escape the containing source directory: a `//go:embed ../web/...` from
// inside internal/web/ is rejected at compile time. Hosting the directives
// in this tiny sibling package keeps the asset paths colocated with the
// templates/static files themselves and lets internal/web import the FS
// values directly.
package web

import "embed"

// TemplatesFS holds every HTML template the web binary renders. The FS root
// preserves the templates/ prefix so callers must fs.Sub it (or use a
// "templates/..." pattern) when parsing.
//
//go:embed templates/*.html
var TemplatesFS embed.FS

// StaticFS holds every static asset served under /static/. The FS root
// preserves the static/ prefix; callers wire it via fs.Sub before handing it
// to http.FileServer so request paths line up cleanly after StripPrefix.
//
//go:embed static/*
var StaticFS embed.FS
