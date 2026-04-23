package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/yalexaner/yacht/internal/share"
)

// shareCookieMaxAge bounds the lifetime of the per-share unlock cookie. Five
// minutes is long enough for a user to page around after unlocking (refresh,
// hit Download, reopen the tab) without having to re-enter the password, and
// short enough that a shared machine doesn't leave a lingering skip-token for
// the next occupant. Phase 9 replaces this cookie with a real signed session
// and can tune the window without changing the handler contract.
const shareCookieMaxAge = 5 * time.Minute

// fileShareView is the data contract between shareHandler and
// share_file.html. Handlers precompute Filename/Size/ExpiresAt as plain
// values so the template never has to deref pointers — the Share struct
// carries several *string / *int64 columns whose nil-checking boilerplate
// belongs in Go code, not HTML.
type fileShareView struct {
	ID        string
	Filename  string
	Size      int64
	ExpiresAt time.Time
}

// textShareView is the share_text.html counterpart: a text share's content
// is pulled out of its *string column and passed as a plain string so the
// template can drop it straight into a <pre> block. html/template autoescapes
// the body, so user-supplied "<script>..." text becomes escaped HTML —
// Phase 8 adds explicit regression coverage.
type textShareView struct {
	ID        string
	Content   string
	ExpiresAt time.Time
}

// passwordPromptView feeds password.html. ID is used to build the POST
// action URL; Error is the human-readable message shown above the input
// when a previous submission was rejected (empty on first display).
type passwordPromptView struct {
	ID    string
	Error string
}

// errorView feeds error.html. Title lands in <title> and <h1>; Message is
// the body paragraph below. Both strings are user-facing — keep them short
// and free of internal detail.
type errorView struct {
	Title   string
	Message string
}

// healthzHandler is the liveness probe: no DB ping, no storage ping, just
// proves the process accepted the TCP connection and routed the request. A
// 200 here tells a health checker (Caddy, Docker, uptime monitor) that the
// binary is up — deeper checks belong in readiness endpoints we haven't
// built yet.
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// notImplementedHandler is a placeholder the mux uses for routes that later
// Phase-7 tasks will flesh out. We return 501 rather than 404 so that a
// half-deployed binary produces a clearly diagnostic response instead of
// looking like a missing route.
func (s *Server) notImplementedHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// shareHandler serves GET /{id}: the share landing page. It resolves the
// share through share.Service, maps the sentinel errors to HTTP statuses,
// gates on the per-share password cookie when the share is protected, and
// renders the file or text view template based on the share's Kind.
//
// The password check here is a UX gate, not a security boundary: the actual
// password is verified on POST /{id} (Task 4), which sets the cookie this
// handler reads. Download in Task 5 uses the same check.
func (s *Server) shareHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		// defense-in-depth: the mux pattern "GET /{id}" should never route an
		// empty id here, but returning 404 is the right response if it does.
		s.renderError(w, http.StatusNotFound, "Not Found", "That share does not exist.")
		return
	}

	sh, err := s.share.Get(r.Context(), id)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}

	if sh.PasswordHash != nil && !s.hasShareCookie(r, id) {
		s.render(w, http.StatusUnauthorized, "password.html", passwordPromptView{ID: id})
		return
	}

	switch sh.Kind {
	case share.KindFile:
		s.render(w, http.StatusOK, "share_file.html", fileShareView{
			ID:        sh.ID,
			Filename:  stringOrEmpty(sh.OriginalFilename),
			Size:      int64OrZero(sh.SizeBytes),
			ExpiresAt: sh.ExpiresAt,
		})
	case share.KindText:
		s.render(w, http.StatusOK, "share_text.html", textShareView{
			ID:        sh.ID,
			Content:   stringOrEmpty(sh.TextContent),
			ExpiresAt: sh.ExpiresAt,
		})
	default:
		// a row with an unknown kind means the DB was written by code we
		// don't own or someone edited rows by hand; log and fail loud so the
		// operator investigates rather than silently rendering an empty page.
		s.logger.Error("unknown share kind", "id", id, "kind", sh.Kind)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "We could not display this share.")
	}
}

// passwordHandler serves POST /{id}: the password form submission. It
// resolves the share, maps the same sentinel errors as shareHandler, verifies
// the plaintext against the stored bcrypt hash, and — on success — sets the
// short-lived unlock cookie and redirects to GET /{id} (POST-redirect-GET).
//
// A share without a stored hash reaching this handler is a caller-side bug
// (the share page never renders the prompt for it), but we return 400 rather
// than silently redirect so a future regression that starts posting to
// unprotected shares surfaces instead of leaking a cookie.
//
// Cookie scope: Path=/, SameSite=Strict, HttpOnly, Max-Age=300. No Secure
// flag in Phase 7 — the cookie value is the literal "1" and carries no
// secret; the real gate is bcrypt, and Phase 14 polish can revisit.
func (s *Server) passwordHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.renderError(w, http.StatusNotFound, "Not Found", "That share does not exist.")
		return
	}

	sh, err := s.share.Get(r.Context(), id)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}

	if sh.PasswordHash == nil {
		// defense-in-depth: the share page never shows the prompt for an
		// unprotected share, so reaching here means a client posted directly.
		// Surface as 400 rather than silently proceeding so the oddity shows
		// up in logs instead of landing a cookie on every visitor.
		s.renderError(w, http.StatusBadRequest, "Bad Request", "This share is not password protected.")
		return
	}

	if err := r.ParseForm(); err != nil {
		s.logger.Error("password form parse failed", "id", id, "err", err)
		s.renderError(w, http.StatusBadRequest, "Bad Request", "Could not read the submitted form.")
		return
	}
	plaintext := r.PostForm.Get("password")

	err = s.share.VerifyPassword(sh, plaintext)
	switch {
	case err == nil:
		// success — set the unlock cookie and send the browser back to the
		// share page via POST-redirect-GET. The GET sees the cookie via
		// hasShareCookie and renders the share view.
		http.SetCookie(w, &http.Cookie{
			Name:     shareCookieName(id),
			Value:    "1",
			Path:     "/",
			MaxAge:   int(shareCookieMaxAge.Seconds()),
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/"+id, http.StatusSeeOther)
		return
	case errors.Is(err, share.ErrPasswordMismatch):
		s.render(w, http.StatusUnauthorized, "password.html", passwordPromptView{
			ID:    id,
			Error: "Incorrect password",
		})
		return
	default:
		s.logger.Error("verify password failed", "id", id, "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}
}

// renderShareError maps share.Service sentinel errors to HTTP status codes
// and the error.html template. Any unrecognized error is logged and surfaced
// as a generic 500 so internal detail never leaks into the response body.
func (s *Server) renderShareError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, share.ErrNotFound):
		s.renderError(w, http.StatusNotFound, "Not Found", "That share does not exist.")
	case errors.Is(err, share.ErrExpired):
		s.renderError(w, http.StatusGone, "Gone", "This share has expired.")
	default:
		s.logger.Error("share lookup failed", "method", r.Method, "path", r.URL.Path, "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
	}
}

// renderError is the single entry point that writes the error.html template.
// Centralizing the render keeps the error layout consistent across handlers
// and gives us one spot to add logging or metrics in a later phase.
func (s *Server) renderError(w http.ResponseWriter, status int, title, message string) {
	s.render(w, status, "error.html", errorView{Title: title, Message: message})
}

// shareCookieName returns the per-share trust-cookie name set by the
// password handler and read here + by the download handler. Phase 9
// replaces this with a signed session cookie; for Phase 7 the value is
// the unsigned literal "1" and its only job is to remember that the
// browser already submitted the correct password for this specific share.
func shareCookieName(id string) string {
	return "yacht_share_" + id
}

// hasShareCookie reports whether r carries a valid password-unlock cookie
// for shareID. The cookie is a UX skip-token, not a credential — the real
// gate is bcrypt on POST — so any present-and-equal-to-"1" cookie counts.
func (s *Server) hasShareCookie(r *http.Request, shareID string) bool {
	c, err := r.Cookie(shareCookieName(shareID))
	if err != nil {
		return false
	}
	return c.Value == "1"
}

// stringOrEmpty dereferences a *string and returns "" when nil. Used to
// convert share.Share's nullable string columns into template-friendly
// plain strings.
func stringOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// int64OrZero dereferences a *int64 and returns 0 when nil. Used to convert
// share.Share.SizeBytes (nullable for text shares) into a plain int64 the
// humanBytes template function can format.
func int64OrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
