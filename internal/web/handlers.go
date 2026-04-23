package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/storage"
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

// downloadHandler serves GET /d/{id}: the actual download stream. It reuses
// shareHandler's error mapping (404/410/500) and password-cookie gate, then
// branches on Kind: file shares stream through storage with a UTF-8-aware
// Content-Disposition; text shares serialize the stored text_content column
// as a plain-text attachment named {shareID}.txt.
//
// A password-protected share reached without a valid unlock cookie renders
// the password prompt at 401 rather than redirecting — the user came
// straight to /d/, so a redirect would bounce them away from the URL they
// clicked.
//
// IncrementDownloadCount runs after the response body starts streaming. We
// use a detached context via context.WithoutCancel so the counter still
// bumps when the client disconnects mid-stream — the byte it already
// received is a completed download from our perspective, and the operator
// metric is more useful when it tracks attempts, not just clean finishes.
// A failure on the counter is logged and swallowed because the user's
// download already succeeded; poisoning the response at this point would
// only confuse them.
func (s *Server) downloadHandler(w http.ResponseWriter, r *http.Request) {
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

	if sh.PasswordHash != nil && !s.hasShareCookie(r, id) {
		s.render(w, http.StatusUnauthorized, "password.html", passwordPromptView{ID: id})
		return
	}

	switch sh.Kind {
	case share.KindFile:
		s.streamFileShare(w, r, sh)
	case share.KindText:
		s.streamTextShare(w, r, sh)
	default:
		s.logger.Error("unknown share kind", "id", id, "kind", sh.Kind)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "We could not display this share.")
	}
}

// streamFileShare pipes the storage-backed payload to the client with the
// headers a browser needs to save it with the uploader's original filename.
// Separated from downloadHandler so the password/kind branches stay
// readable; no external caller reaches this method.
func (s *Server) streamFileShare(w http.ResponseWriter, r *http.Request, sh *share.Share) {
	body, err := s.share.OpenContent(r.Context(), sh)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// db says the share exists, storage says the object doesn't — a
			// drift state that warrants an operator alert, but to the user
			// looks indistinguishable from any other internal failure.
			s.logger.Warn("storage object missing for share", "id", sh.ID, "err", err)
			s.renderError(w, http.StatusInternalServerError, "Something went wrong", "The backing data for this share is unavailable.")
			return
		}
		s.logger.Error("open content failed", "id", sh.ID, "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}
	defer body.Close()

	filename := stringOrEmpty(sh.OriginalFilename)
	mime := stringOrEmpty(sh.MIMEType)
	size := int64OrZero(sh.SizeBytes)

	if mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.Header().Set("Content-Disposition", rfc5987ContentDisposition(filename))
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, body); err != nil {
		// once headers are written the connection is already committed; all
		// we can do is log the partial transfer for operator visibility.
		s.logger.Warn("download stream interrupted", "id", sh.ID, "err", err)
	}

	s.bumpDownloadCount(r, sh.ID)
}

// streamTextShare serializes the stored text_content column as a plain-text
// attachment. The filename is the share id with a .txt suffix — RFC 5987
// isn't needed because share ids are ASCII-only by construction.
func (s *Server) streamTextShare(w http.ResponseWriter, r *http.Request, sh *share.Share) {
	content := stringOrEmpty(sh.TextContent)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, sh.ID+".txt"))
	w.WriteHeader(http.StatusOK)

	if _, err := io.WriteString(w, content); err != nil {
		s.logger.Warn("text download stream interrupted", "id", sh.ID, "err", err)
	}

	s.bumpDownloadCount(r, sh.ID)
}

// bumpDownloadCount runs after the response body has started streaming.
// Uses a detached context with a short timeout so a cancelled request ctx
// (client disconnect, shutdown) doesn't suppress the counter update — the
// download bytes have already left our process, so the download has
// functionally happened regardless of whether the client stuck around to
// read them. Errors are logged and swallowed because the response is
// already committed and surfacing a counter failure would only mislead
// the user.
func (s *Server) bumpDownloadCount(r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.share.IncrementDownloadCount(ctx, id); err != nil {
		s.logger.Warn("increment download count", "id", id, "err", err)
	}
}

// rfc5987ContentDisposition builds the Content-Disposition header value for
// an attachment with the supplied filename. The filename*=UTF-8'' form is
// the canonical way to encode non-ASCII filenames per RFC 5987; we also
// emit an ASCII-only fallback filename= for HTTP clients that ignore the
// extended parameter. Non-ASCII bytes in the fallback are replaced with
// underscores so the header stays valid under the stricter RFC 6266 token
// rules.
func rfc5987ContentDisposition(name string) string {
	return fmt.Sprintf(`attachment; filename=%q; filename*=UTF-8''%s`, asciiFallbackName(name), rfc5987Encode(name))
}

// rfc5987Encode percent-encodes name per RFC 5987 §3.2.1: the "attr-char"
// set (ALPHA / DIGIT / "!" / "#" / "$" / "&" / "+" / "-" / "." / "^" / "_"
// / "`" / "|" / "~") passes through; everything else — including space and
// every non-ASCII byte — is written as %HH over the UTF-8 byte sequence.
func rfc5987Encode(name string) string {
	var out []byte
	for i := 0; i < len(name); i++ {
		b := name[i]
		if isAttrChar(b) {
			out = append(out, b)
			continue
		}
		out = append(out, '%')
		const hex = "0123456789ABCDEF"
		out = append(out, hex[b>>4], hex[b&0x0F])
	}
	return string(out)
}

// asciiFallbackName returns a filename safe to place inside the quoted
// filename= parameter: non-ASCII bytes and the two quoted-string specials
// (`"` and `\`) collapse to underscores so the header stays RFC 6266
// compliant when a client ignores the filename* extension.
func asciiFallbackName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b >= 0x7f || b == '"' || b == '\\' {
			out = append(out, '_')
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return "download"
	}
	return string(out)
}

// isAttrChar reports whether b is in the RFC 5987 attr-char set.
func isAttrChar(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
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
