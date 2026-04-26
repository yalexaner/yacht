package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/yalexaner/yacht/internal/i18n"
)

// templateFuncs lists every helper the HTML templates reach for. Registered
// on the base template before parsing so all cloned-per-page sets inherit
// the same FuncMap.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"humanBytes": humanBytes,
		"T":          i18n.T,
	}
}

// humanBytes renders n in the largest unit that keeps the value >= 1, with
// one decimal place above 1 KiB and a plain "N B" below. The shape matches
// the bot's humanizeBytes — duplicated rather than shared because the web
// and bot template surfaces may drift independently and a shared helper
// would couple their formatting decisions prematurely.
func humanBytes(n int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// pageTemplates is the set of page files (keyed by basename) Phase 7 ships.
// Each page contributes a {{define "content"}} block that the shared base
// layout pulls in. Keeping the list explicit here — rather than discovering
// it via a glob — means a forgotten file surfaces as a startup error the
// first time the binary boots, not as a 500 on the first user request.
var pageTemplates = []string{
	"share_file.html",
	"share_text.html",
	"password.html",
	"error.html",
	"login.html",
	"home.html",
	"upload.html",
	"share_created.html",
}

// parseTemplates builds one fully-assembled template per page: a clone of
// the base layout merged with the page's content/title overrides. Cloning is
// the idiomatic way to let multiple pages share a single base without their
// {{define "content"}} blocks clobbering each other inside a single parse
// set.
//
// The returned map is keyed by page filename (share_file.html, etc.).
// Callers execute the "base.html" template within the looked-up entry, which
// triggers the page's "content" override inside the base shell.
func parseTemplates(fsys fs.FS) (map[string]*template.Template, error) {
	base, err := template.New("base.html").Funcs(templateFuncs()).ParseFS(fsys, "base.html")
	if err != nil {
		return nil, fmt.Errorf("parse base template: %w", err)
	}

	out := make(map[string]*template.Template, len(pageTemplates))
	for _, name := range pageTemplates {
		clone, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone base template for %q: %w", name, err)
		}
		tmpl, err := clone.ParseFS(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", name, err)
		}
		out[name] = tmpl
	}
	return out, nil
}

// render executes the requested page template into a buffer before touching
// the response so a mid-render failure can still return a clean 500 instead
// of a 2xx with truncated HTML. Buffering is cheap — every Phase-7 page is
// well under 10 KB — and the correctness win is worth it.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	tmpl, ok := s.templates[name]
	if !ok {
		s.logger.Error("unknown template", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base.html", data); err != nil {
		s.logger.Error("render template", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
