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
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/web/middleware"
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
	cfg          *config.Web
	db           *sql.DB
	share        *share.Service
	authTelegram *auth.TelegramWidget
	authBotToken *auth.BotToken
	templates    map[string]*template.Template
	static       fs.FS
	logger       *slog.Logger
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
func New(
	cfg *config.Web,
	db *sql.DB,
	share *share.Service,
	authTelegram *auth.TelegramWidget,
	authBotToken *auth.BotToken,
	logger *slog.Logger,
) (*Server, error) {
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
		cfg:          cfg,
		db:           db,
		share:        share,
		authTelegram: authTelegram,
		authBotToken: authBotToken,
		templates:    tmpl,
		static:       staticFS,
		logger:       logger,
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

	mux.HandleFunc("GET /login", s.loginHandler)
	mux.HandleFunc("GET /auth/telegram/callback", s.telegramCallbackHandler)
	mux.HandleFunc("GET /auth/{token}", s.botTokenHandler)
	mux.HandleFunc("POST /logout", s.logoutHandler)

	// GET / is gated behind RequireAuth so the post-login redirect lands
	// somewhere meaningful: an authed visitor sees the placeholder home,
	// an unauthed one is bounced back to /login by the middleware (which
	// is exactly the loop we want — without this route, "/" would 404 and
	// the login flow would look broken on success).
	mux.Handle("GET /{$}", s.RequireAuth()(http.HandlerFunc(s.homeHandler)))

	// Upload routes are gated behind RequireAuth: only logged-in operators
	// mint shares. The download/share-page routes below stay public so a
	// recipient with a link can fetch without an account.
	mux.Handle("GET /upload", s.RequireAuth()(http.HandlerFunc(s.uploadFormHandler)))

	mux.HandleFunc("GET /{id}", s.shareHandler)
	mux.HandleFunc("POST /{id}", s.passwordHandler)
	mux.HandleFunc("GET /d/{id}", s.downloadHandler)

	return s.logMiddleware(mux)
}

// RequireAuth returns the session-gate middleware preconfigured with this
// server's DB handle and the configured session cookie name. Phase 9 defines
// and tests the middleware but does not apply it to any route; Phase 10's
// upload handlers are the first consumer and reach in via this accessor so
// they don't need to know which cookie name or DB handle Server owns.
func (s *Server) RequireAuth() func(http.Handler) http.Handler {
	return middleware.RequireAuth(s.db, s.cfg.SessionCookieName)
}

// statusRecorder snapshots the outgoing HTTP status so logMiddleware can log
// it after the handler returns. http.ResponseWriter doesn't expose the status
// once WriteHeader runs, and a handler that never calls WriteHeader relies on
// the stdlib's implicit-200-on-first-Write contract — we mirror that here so
// the logged value matches what the client actually receives.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// logMiddleware emits one INFO line per request with method, path, status,
// and wall-clock duration. It's the only observability surface Phase 13 has
// until proper metrics land, so every route — including /healthz — goes
// through it; noisy liveness probes are preferable to a deployment where we
// can't tell whether a request ever reached the binary.
//
// Paths are run through sanitizePathForLog so one-time login tokens in the
// /auth/{token} flow don't land in logs as usable credentials.
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.logger.Info("request",
			"method", r.Method,
			"path", sanitizePathForLog(r.URL.Path),
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}

// sanitizePathForLog redacts the secret segment of /auth/{token} URLs so a
// one-time login token never lands in request logs. The widget callback
// path /auth/telegram/callback carries no secret in its path (the signed
// fields ride in the query string, which logMiddleware never logs) and is
// returned unchanged. Every other /auth/<segment> shape collapses to
// /auth/[REDACTED] because the bot-token route uses a single path segment
// as the credential itself.
func sanitizePathForLog(path string) string {
	const authPrefix = "/auth/"
	if !strings.HasPrefix(path, authPrefix) {
		return path
	}
	rest := path[len(authPrefix):]
	if rest == "" || rest == "telegram/callback" {
		return path
	}
	return authPrefix + "[REDACTED]"
}
