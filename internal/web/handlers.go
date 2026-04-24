package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/storage"
	"github.com/yalexaner/yacht/internal/web/middleware"
)

// shareCookieMaxAge bounds the lifetime of the per-share unlock cookie. Five
// minutes is long enough for a user to page around after unlocking (refresh,
// hit Download, reopen the tab) without having to re-enter the password, and
// short enough that a shared machine doesn't leave a lingering skip-token for
// the next occupant. Phase 9 replaces this cookie with a real signed session
// and can tune the window without changing the handler contract.
const shareCookieMaxAge = 5 * time.Minute

// passwordFormMaxBytes caps the password POST body so an attacker can't
// tie up memory streaming gigabytes into ParseForm. A few KB is an order of
// magnitude above any legitimate password submission and leaves no room for
// mistakes when the form gains fields in later phases.
const passwordFormMaxBytes = 4 * 1024

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

// homeView feeds home.html. DisplayName is rendered into the greeting
// when the joined users row carries one; an empty string falls back to a
// generic "Welcome." line so the page stays well-formed for accounts that
// never set a Telegram display name.
type homeView struct {
	DisplayName string
}

// loginView feeds login.html. BotUsername is the Telegram bot username used
// both by the Login Widget's data-telegram-login attribute and by the
// fallback text's @username link. Error is the human-readable message shown
// when the login flow redirected the user back with an ?error= code; empty
// on first display.
type loginView struct {
	BotUsername string
	Error       string
}

// loginErrorMessages maps ?error= query codes produced by the auth handlers
// (Tasks 7-9) to human-readable messages. Unknown codes resolve to an empty
// string so an attacker cannot inject arbitrary text through the query
// param — even though html/template would auto-escape it, not echoing
// untrusted strings at all is the simpler guarantee.
var loginErrorMessages = map[string]string{
	"invalid_signature": "The Telegram login signature did not verify. Please try again.",
	"access_denied":     "Access denied — your Telegram account is not authorized to log in.",
	"invalid_link":      "That login link is not valid.",
	"link_expired":      "Your login link has expired. Send /weblogin to the bot for a fresh one.",
	"link_used":         "That login link has already been used.",
}

// homeHandler serves GET /: the post-login landing page. Wired behind
// RequireAuth at the mux, so by the time we get here the request carries
// a hydrated *auth.User in its context — a cookie-less or expired-session
// visitor was already bounced to /login by the middleware. The defensive
// branch covers a routing regression that ever lets the handler run
// without the gate.
//
// Phase 9 only needs a placeholder home: a greeting plus a logout form.
// Phase 10's upload UI replaces this view with the real composer; the
// route binding stays the same so the redirect target after login keeps
// working through that swap.
func (s *Server) homeHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "home.html", homeView{DisplayName: user.DisplayName})
}

// loginHandler serves GET /login: the login page with the Telegram Login
// Widget plus a fallback block pointing the user at the /weblogin bot
// command for networks where the widget is blocked. An optional ?error=
// query param surfaces a banner above the widget — only codes listed in
// loginErrorMessages render anything; arbitrary values are dropped on the
// floor so the page never echoes untrusted text.
func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "login.html", loginView{
		BotUsername: s.cfg.TelegramBotUsername,
		Error:       loginErrorMessages[r.URL.Query().Get("error")],
	})
}

// telegramCallbackHandler serves GET /auth/telegram/callback: the endpoint
// the Telegram Login Widget posts signed user fields to via browser GET when
// configured with data-auth-url. Verify re-derives the HMAC and resolves the
// embedded Telegram ID to an admin *User; the two expected failure modes
// (ErrInvalidSignature, ErrUnauthorized) map to specific ?error= codes on the
// login page so the user sees a meaningful message. Any other error is an
// internal fault — log + generic 500.
//
// On success we mint a session, set the yacht_session cookie, and redirect
// to "/" with 303 See Other (POST-redirect-GET semantics — even though this
// is a GET, See Other is the idiomatic "auth completed, go home" code).
func (s *Server) telegramCallbackHandler(w http.ResponseWriter, r *http.Request) {
	user, err := s.authTelegram.Verify(r)
	switch {
	case err == nil:
		// fall through to session creation below.
	case errors.Is(err, auth.ErrInvalidSignature):
		http.Redirect(w, r, "/login?error=invalid_signature", http.StatusSeeOther)
		return
	case errors.Is(err, auth.ErrUnauthorized):
		http.Redirect(w, r, "/login?error=access_denied", http.StatusSeeOther)
		return
	default:
		s.logger.Error("telegram widget verify", "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}

	sessionID, err := auth.CreateSession(r.Context(), s.db, user.ID, s.authTelegram.Name(), s.cfg.SessionLifetime)
	if err != nil {
		s.logger.Error("create session (telegram widget)", "user_id", user.ID, "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionLifetime.Seconds()),
		HttpOnly: true,
		// SameSite=Lax (not Strict) because the login flow's redirect lands
		// via Telegram's widget iframe — Strict would drop the cookie on
		// top-level navigations initiated from another origin. Lax still
		// blocks cross-site POSTs, which is what we care about for /logout.
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// botTokenHandler serves GET /auth/{token}: the one-time login link the bot
// DMs a user in response to /weblogin. ConsumeLoginTokenTx atomically
// validates the token and flips used_at on the row, so an immediate second
// click gets ErrTokenUsed rather than re-authenticating. The four expected
// failure modes each map to a distinct ?error= code on the login page —
// the user needs a different message for "typo" vs "link expired" vs
// "already clicked" vs "your account is not authorized".
//
// The token consume and the session insert run inside a single transaction
// so a session-INSERT failure rolls back the used_at flip — without the
// transaction, a transient DB error after the consume would permanently
// burn the link and leave the user rate-limited (60s) on /weblogin for a
// fault that wasn't theirs.
//
// /auth/{token} is unauthenticated, so a cheap read-only existence check
// runs first. _txlock=immediate makes BeginTx grab the SQLite writer slot
// up front; without the pre-check, random invalid-token probes would
// reserve that slot for the duration of every fuzzed URL and queue
// unrelated writes from both binaries. The pre-check uses a pooled reader
// (no writer lock), and the conditional UPDATE inside ConsumeLoginTokenTx
// still owns the atomic single-use guarantee — so a token that exists at
// pre-check time but is consumed by another caller before the tx opens
// still resolves to ErrTokenUsed for the loser.
//
// On success we mint a session tied to the bot_token provider, set the
// yacht_session cookie (same attributes as the telegram widget path), and
// redirect to "/" with 303 See Other.
func (s *Server) botTokenHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	if err := s.authBotToken.LoginTokenExists(r.Context(), token); err != nil {
		switch {
		case errors.Is(err, auth.ErrTokenNotFound):
			http.Redirect(w, r, "/login?error=invalid_link", http.StatusSeeOther)
		default:
			s.logger.Error("bot token exists check", "err", err)
			s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		}
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.logger.Error("begin tx (bot token)", "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}
	// Rollback is a no-op after a successful Commit, so the defer is safe
	// across both happy- and error-paths and removes the every-branch
	// boilerplate that would otherwise leak a tx on an early return.
	defer func() { _ = tx.Rollback() }()

	user, err := s.authBotToken.ConsumeLoginTokenTx(r.Context(), tx, token)
	switch {
	case err == nil:
		// fall through to session creation below.
	case errors.Is(err, auth.ErrTokenNotFound):
		http.Redirect(w, r, "/login?error=invalid_link", http.StatusSeeOther)
		return
	case errors.Is(err, auth.ErrTokenExpired):
		http.Redirect(w, r, "/login?error=link_expired", http.StatusSeeOther)
		return
	case errors.Is(err, auth.ErrTokenUsed):
		http.Redirect(w, r, "/login?error=link_used", http.StatusSeeOther)
		return
	case errors.Is(err, auth.ErrUnauthorized):
		http.Redirect(w, r, "/login?error=access_denied", http.StatusSeeOther)
		return
	default:
		s.logger.Error("bot token consume", "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}

	sessionID, err := auth.CreateSessionTx(r.Context(), tx, user.ID, s.authBotToken.Name(), s.cfg.SessionLifetime)
	if err != nil {
		s.logger.Error("create session (bot token)", "user_id", user.ID, "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}

	if err := tx.Commit(); err != nil {
		s.logger.Error("commit (bot token)", "user_id", user.ID, "err", err)
		s.renderError(w, http.StatusInternalServerError, "Something went wrong", "An internal error occurred.")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionLifetime.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logoutHandler serves POST /logout: deletes the caller's server-side
// session row, clears the yacht_session cookie in the response, and sends
// the browser back to /login with 303 See Other.
//
// POST (not GET) because a GET /logout would trigger on any link prefetch,
// image load, or browser preload — a trivial cross-origin <img src=...> tag
// could log the user out. The SameSite=Lax cookie blocks cross-origin POSTs
// by default, so a form submission from our own login page is the only way
// to reach this handler in the happy path.
//
// DeleteSession errors are logged and swallowed: the cookie gets cleared
// regardless, so the user's browser is in the right state. A DB failure
// here at worst leaves an orphan session row that the cleanup worker
// removes the next time it runs.
//
// No cookie on the request is a legitimate case — a stale tab, a user who
// never logged in, a bot probing the endpoint — so we redirect to /login
// without fanfare rather than surfacing an error.
func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cfg.SessionCookieName); err == nil && c.Value != "" {
		if err := auth.DeleteSession(r.Context(), s.db, c.Value); err != nil {
			s.logger.Warn("delete session on logout", "err", err)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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

// shareHandler serves GET /{id}: the share landing page. It resolves the
// share through share.Service, maps the sentinel errors to HTTP statuses,
// gates on the per-share password cookie when the share is protected, and
// renders the file or text view template based on the share's Kind.
//
// The password itself is verified on POST /{id} (Task 4), which sets the
// unlock cookie this handler reads. The cookie value is a SHA-256 of the
// per-share bcrypt hash, so only a client that has completed the POST flow
// can carry a valid one — a visitor who merely knows the share ID is still
// blocked at the prompt. Download in Task 5 uses the same check.
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

	if sh.PasswordHash != nil && !s.hasShareCookie(r, sh) {
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
// Cookie scope: Path=/, SameSite=Strict, HttpOnly, Max-Age=300, Secure when
// the request arrived over TLS (direct r.TLS or X-Forwarded-Proto=https from
// the Phase-13 Caddy front). The cookie value is a real bearer token derived
// from the bcrypt hash, so a plaintext-HTTP leak would let an on-path
// attacker replay it for the 5-minute window — Secure is what prevents that.
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

	r.Body = http.MaxBytesReader(w, r.Body, passwordFormMaxBytes)
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
		// hasShareCookie and renders the share view. The cookie value is
		// derived from the stored bcrypt hash so it can only be produced
		// after a real password verification — a client that merely guesses
		// the share ID cannot forge it.
		http.SetCookie(w, &http.Cookie{
			Name:     shareCookieName(id),
			Value:    shareCookieToken(*sh.PasswordHash),
			Path:     "/",
			MaxAge:   int(shareCookieMaxAge.Seconds()),
			HttpOnly: true,
			Secure:   requestIsTLS(r),
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

// shareCookieName returns the per-share unlock-cookie name set by the
// password handler and read here + by the download handler. Phase 9
// replaces this with a signed session cookie; for Phase 7 the value is
// shareCookieToken(passwordHash) — a fingerprint of the per-share bcrypt
// hash, which a client who knows only the share ID cannot produce.
func shareCookieName(id string) string {
	return "yacht_share_" + id
}

// shareCookieToken derives the unlock-cookie value from the share's stored
// bcrypt hash. bcrypt generates a fresh random salt per hash, so the output
// is unique per share and unreachable to anyone without the hash itself.
// Taking SHA-256 of the hash and truncating to 16 bytes (32 hex chars) keeps
// the cookie small while leaving 128 bits of collision resistance — orders
// of magnitude beyond any realistic forgery attempt. Phase 9 replaces this
// with a proper signed session cookie.
func shareCookieToken(passwordHash string) string {
	sum := sha256.Sum256([]byte(passwordHash))
	return hex.EncodeToString(sum[:16])
}

// requestIsTLS reports whether the request arrived over TLS — either
// directly (r.TLS != nil) or via a reverse proxy that terminated TLS and
// signalled it with X-Forwarded-Proto=https. The Phase-13 Caddy front sets
// this header authoritatively; trusting it from arbitrary clients is fine
// here because the worst an attacker achieves by forging it is marking their
// own Set-Cookie as Secure, which is a self-DOS, not an escalation.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// hasShareCookie reports whether r carries a valid password-unlock cookie
// for sh. The cookie value must match shareCookieToken(*sh.PasswordHash)
// byte-for-byte; comparing in constant time keeps a timing oracle off the
// table. Returns false for shares without a password set — callers should
// only reach this check when sh.PasswordHash != nil, but defending here
// keeps the contract explicit.
func (s *Server) hasShareCookie(r *http.Request, sh *share.Share) bool {
	if sh.PasswordHash == nil {
		return false
	}
	c, err := r.Cookie(shareCookieName(sh.ID))
	if err != nil {
		return false
	}
	expected := shareCookieToken(*sh.PasswordHash)
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(expected)) == 1
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

	if sh.PasswordHash != nil && !s.hasShareCookie(r, sh) {
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
	// nosniff neutralizes the MIME-sniff fallback that lets some browsers
	// render an uploaded .html/.svg file inline despite Content-Disposition:
	// attachment. Uploader-supplied mime types reach clients unchanged, so a
	// malicious upload without this header could be a drive-by XSS surface.
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
