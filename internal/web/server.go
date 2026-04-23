// Package web is the web binary's HTTP layer. Phase 7 ships the download
// side only — share page, password prompt, download stream, health check.
// Upload, auth, and i18n come in Phases 9-11.
//
// Construction is deferred to cmd/web/main.go: callers build a
// share.Service first, then wire it into web.New alongside the loaded
// config. New parses the embedded HTML templates and binds the static
// asset FS up front so any malformed template fails the binary at boot
// rather than on the first request. Routes() exposes the http.Handler that
// http.Server.ListenAndServe consumes.
package web

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/share"
	webassets "github.com/yalexaner/yacht/web"
)

// Server is the HTTP layer's per-process state. It owns the parsed template
// set and the rooted static FS so handlers can render and serve without
// re-parsing on every request. Construct via New; callers feed Server.Routes
// to a real *http.Server in cmd/web/main.go.
//
// The struct is intentionally unexported field-by-field: white-box tests in
// the same package reach in directly, and external callers have no business
// poking at the internals — the public surface is New + Routes.
type Server struct {
	cfg       *config.Web
	share     *share.Service
	templates map[string]*template.Template
	static    fs.FS
	logger    *slog.Logger
}

// New parses the embedded HTML templates, sub-roots the static FS to drop
// the static/ prefix (so http.FileServer + StripPrefix line up cleanly),
// and returns the assembled Server. Template-parse failure here aborts
// startup — that's the whole reason for parsing eagerly: a malformed
// template should crash the binary at boot, not 500 on the first user
// request.
//
// The fs.Sub calls cannot fail for the static patterns we ship (the embed
// directive guarantees the prefixes exist), but we still propagate the
// error rather than swallow it: a future refactor that drops one of those
// directives would otherwise produce a confusing nil-FS panic deeper in
// the request path.
func New(cfg *config.Web, share *share.Service, logger *slog.Logger) (*Server, error) {
	tmplFS, err := fs.Sub(webassets.TemplatesFS, "templates")
	if err != nil {
		return nil, fmt.Errorf("web.New: templates sub fs: %w", err)
	}
	tmpl, err := parseTemplates(tmplFS)
	if err != nil {
		return nil, fmt.Errorf("web.New: %w", err)
	}

	staticFS, err := fs.Sub(webassets.StaticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("web.New: static sub fs: %w", err)
	}

	return &Server{
		cfg:       cfg,
		share:     share,
		templates: tmpl,
		static:    staticFS,
		logger:    logger,
	}, nil
}

// Routes assembles the HTTP handler that cmd/web feeds to http.Server. It
// uses Go 1.22 pattern routing so method and path live in the pattern
// string; a bare "/{id}" is necessarily GET because POST uses the explicit
// "POST /{id}" entry. Routes that later Phase-7 tasks fill in are bound to
// a 501 placeholder so a partially-deployed binary fails loud instead of
// 404-ing.
//
// Static assets are served by http.FileServer over the sub-rooted FS that
// New already stripped; StripPrefix peels the "/static/" segment so the
// file server resolves "/static/style.css" against "style.css" inside the
// FS — anything missing produces a plain 404 without touching the rest of
// the handler stack.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthzHandler)

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.static))))

	mux.HandleFunc("GET /{id}", s.shareHandler)
	mux.HandleFunc("POST /{id}", s.passwordHandler)
	mux.HandleFunc("GET /d/{id}", s.downloadHandler)

	return mux
}
