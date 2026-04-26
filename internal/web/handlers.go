package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/i18n"
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

// langCookieName is the cookie the language switcher writes and the lang
// middleware reads. Hard-coded (not config-driven) because the value is also
// the public face of /lang/{code} — flipping the name would mean coordinated
// changes in templates, JS, and the middleware default, with no operator
// upside.
const langCookieName = "yacht_lang"

// langCookieMaxAge bounds the language preference cookie's lifetime. One year
// matches the SPEC's "the user picked a language, remember it" intent — the
// preference is sticky enough that a returning visitor weeks later still sees
// their last pick without being forced through the switcher again. For a
// logged-in user, users.lang carries the preference past cookie expiry, so
// the cookie length is really a UX knob for anonymous visitors.
const langCookieMaxAge = 365 * 24 * time.Hour

// fileShareView is the data contract between shareHandler and
// share_file.html. Handlers precompute Filename/Size/ExpiresAt as plain
// values so the template never has to deref pointers — the Share struct
// carries several *string / *int64 columns whose nil-checking boilerplate
// belongs in Go code, not HTML.
type fileShareView struct {
	Lang      string
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
	Lang      string
	ID        string
	Content   string
	ExpiresAt time.Time
}

// passwordPromptView feeds password.html. ID is used to build the POST
// action URL; Error is the human-readable message shown above the input
// when a previous submission was rejected (empty on first display).
type passwordPromptView struct {
	Lang  string
	ID    string
	Error string
}

// errorView feeds error.html. Title lands in <title> and <h1>; Message is
// the body paragraph below. Both strings are user-facing — keep them short
// and free of internal detail.
type errorView struct {
	Lang    string
	Title   string
	Message string
}

// homeView feeds home.html. DisplayName is rendered into the greeting
// when the joined users row carries one; an empty string falls back to a
// generic "Welcome." line so the page stays well-formed for accounts that
// never set a Telegram display name.
type homeView struct {
	Lang        string
	DisplayName string
}

// loginView feeds login.html. BotUsername is the Telegram bot username used
// both by the Login Widget's data-telegram-login attribute and by the
// fallback text's @username link. Error is the human-readable message shown
// when the login flow redirected the user back with an ?error= code; empty
// on first display.
type loginView struct {
	Lang        string
	BotUsername string
	Error       string
}

// expiryOption is one entry in the upload form's "Expires after" dropdown.
// BundleKey is the i18n lookup key (form.upload.expiry.NNN); the template
// resolves it via {{ T $.Lang .BundleKey }} so the visible label translates.
// Seconds is the canonical wire value validated by the POST handler.
// Seconds is what the form posts back; Label is what the operator sees.
// Stored as int64 (not time.Duration) because the form value is a string of
// decimal seconds — keeping the same units on both sides means the template
// renders raw integers and the server parses them with strconv.ParseInt
// without a unit conversion in the middle.
type expiryOption struct {
	BundleKey string
	Seconds   int64
}

// expiryOptions is the allowlist of expiry durations the upload form offers
// (Phase 10 plan, decision #5). The POST handler validates the submitted
// `expiry` field against the Seconds values in this slice — anything outside
// the list is rejected. Keeping the list in code (not config) means a future
// "1 year" option needs a deliberate code change rather than a stray env var.
var expiryOptions = []expiryOption{
	{BundleKey: "form.upload.expiry.1h", Seconds: 3600},
	{BundleKey: "form.upload.expiry.6h", Seconds: 21600},
	{BundleKey: "form.upload.expiry.24h", Seconds: 86400},
	{BundleKey: "form.upload.expiry.3d", Seconds: 259200},
	{BundleKey: "form.upload.expiry.7d", Seconds: 604800},
	{BundleKey: "form.upload.expiry.30d", Seconds: 2592000},
}

// uploadFormView feeds upload.html. ExpiryOptions is the dropdown allowlist;
// DefaultExpirySeconds picks the pre-selected option (matched against
// cfg.DefaultExpiry); MaxUploadBytes is rendered as a human-readable cap so
// the operator knows the file limit before they pick one; MaxTextBytes does
// the same for the textarea (different ceiling — text rides the per-field
// cap, files ride MaxUploadBytes). Error is the banner shown above the
// form when a previous submission was rejected.
type uploadFormView struct {
	Lang                 string
	ExpiryOptions        []expiryOption
	DefaultExpirySeconds int64
	MaxUploadBytes       int64
	MaxTextBytes         int64
	Error                string
}

// shareCreatedView feeds share_created.html: the post-upload confirmation
// page. ShareURL is the absolute external link the operator shares with a
// recipient (cfg.BaseURL + "/" + share.ID); the template surfaces it next to
// the copy button. Kind/Filename/Size are populated from the underlying row
// — Filename and Size are zero values for text shares, and the template
// branches on Kind.
type shareCreatedView struct {
	Lang      string
	ID        string
	ShareURL  string
	Kind      string
	Filename  string
	Size      int64
	ExpiresAt time.Time
}

// loginErrorMessages maps ?error= query codes produced by the auth handlers
// to bundle keys. The login handler resolves the key into the visitor's
// language at render time. Unknown codes resolve to an empty string so an
// attacker cannot inject arbitrary text through the query param — even
// though html/template would auto-escape it, not echoing untrusted strings
// at all is the simpler guarantee.
var loginErrorMessages = map[string]string{
	"invalid_signature": "error.auth.invalid_signature",
	"access_denied":     "error.auth.access_denied",
	"invalid_link":      "error.auth.invalid_link",
	"link_expired":      "error.auth.link_expired",
	"link_used":         "error.auth.link_used",
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
	s.render(w, http.StatusOK, "home.html", homeView{
		Lang:        middleware.LangFromContext(r.Context()),
		DisplayName: user.DisplayName,
	})
}

// uploadFormHandler serves GET /upload: the upload form. Wired behind
// RequireAuth at the mux, so by the time we get here the request carries a
// hydrated *auth.User in its context — an unauthed visitor was already
// bounced to /login. The view model carries the allowlist dropdown options,
// the pre-selected default (matched against cfg.DefaultExpiry, falling back
// to 24h when the configured default doesn't appear in the list), and the
// upload size cap so the template can show it humanized.
func (s *Server) uploadFormHandler(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "upload.html", uploadFormView{
		Lang:                 middleware.LangFromContext(r.Context()),
		ExpiryOptions:        expiryOptions,
		DefaultExpirySeconds: defaultExpirySeconds(s.cfg.DefaultExpiry),
		MaxUploadBytes:       s.cfg.MaxUploadBytes,
		MaxTextBytes:         uploadFieldMaxBytes,
	})
}

// renderUploadForm re-renders upload.html with an error banner. Used on
// every parse / validation / size failure so the operator sees the form
// they just submitted come back with a message above it instead of being
// bounced to the generic error template — losing their intent in the
// process. Pulled out of the handler so the various failure branches share
// a single render call site.
func (s *Server) renderUploadForm(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, status, "upload.html", uploadFormView{
		Lang:                 middleware.LangFromContext(r.Context()),
		ExpiryOptions:        expiryOptions,
		DefaultExpirySeconds: defaultExpirySeconds(s.cfg.DefaultExpiry),
		MaxUploadBytes:       s.cfg.MaxUploadBytes,
		MaxTextBytes:         uploadFieldMaxBytes,
		Error:                msg,
	})
}

// uploadSubmitHandler serves POST /upload: takes the parsed form, hands the
// content to share.Service, and redirects to the created-confirmation page.
// Wired behind RequireAuth at the mux, so user is guaranteed present —
// the defensive miss-branch covers a routing regression that ever lets the
// handler run without the gate.
//
// File-size handling (Phase 10 plan, Technical Details): multipart.Part
// carries no per-part Content-Length, and storage backends — R2 in
// particular — require ContentLength up front. We spool the file part to a
// temp file via os.CreateTemp, stat it for size, then re-open and stream
// from disk into share.CreateFileShare. The MaxBytesReader wrap inside
// parseUploadForm bounds the spool to MaxUploadBytes + headroom, so a
// hostile client can't exhaust local disk on this step. Costs one extra
// disk pass per upload; at personal scale and the 100 MB cap the trade-off
// is correct robustness over the streaming-purity alternative.
//
// Failure routing: parse / validation errors and oversized bodies
// re-render upload.html with an Error banner so the operator's intent is
// preserved. Anything else (DB or storage fault) is an internal failure
// and gets the shared error template at 500.
func (s *Server) uploadSubmitHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	lang := middleware.LangFromContext(r.Context())
	fields, err := parseUploadForm(r, s.cfg.MaxUploadBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.renderUploadForm(w, r, http.StatusRequestEntityTooLarge,
				fmt.Sprintf(i18n.T(lang, "error.upload.toolarge"), humanBytes(s.cfg.MaxUploadBytes)))
			return
		}
		// Text-overflow gets its own banner so the operator knows which cap
		// they hit (the text cap is far below MaxUploadBytes; surfacing the
		// generic message would mislead).
		if errors.Is(err, errTextTooLarge) {
			s.renderUploadForm(w, r, http.StatusRequestEntityTooLarge,
				fmt.Sprintf(i18n.T(lang, "error.upload.toolargetext"), humanBytes(uploadFieldMaxBytes)))
			return
		}
		s.logger.Warn("parse upload form", "err", err)
		s.renderUploadForm(w, r, http.StatusBadRequest, i18n.T(lang, "error.upload.parse"))
		return
	}

	var sh *share.Share
	switch fields.Kind {
	case share.KindText:
		sh, err = s.share.CreateTextShare(r.Context(), share.CreateTextOpts{
			UserID:   user.ID,
			Content:  fields.Text,
			Password: fields.Password,
			Expiry:   fields.Expiry,
		})
	case share.KindFile:
		sh, err = s.createFileShareFromPart(r, user.ID, fields)
	}
	if err != nil {
		// MaxBytesError can surface during the spool step too — fields parsed
		// fine, but the file part itself overflowed. Map to the same friendly
		// 413 page rather than the generic 500.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.renderUploadForm(w, r, http.StatusRequestEntityTooLarge,
				fmt.Sprintf(i18n.T(lang, "error.upload.toolarge"), humanBytes(s.cfg.MaxUploadBytes)))
			return
		}
		s.logger.Error("create share", "kind", fields.Kind, "user_id", user.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.upload.failed")
		return
	}

	http.Redirect(w, r, "/shares/"+sh.ID+"/created", http.StatusSeeOther)
}

// createdHandler serves GET /shares/{id}/created: the post-upload
// confirmation page that shows the freshly-minted share's external URL plus
// the copy button. Wired behind RequireAuth at the mux, so user is
// guaranteed present — the defensive miss-branch covers a routing
// regression that ever lets the handler run without the gate.
//
// Owner-only by design: a share row owned by user A must not surface its
// existence to user B's session. We map "not the creator" to 404 (rather
// than 403) so a probing visitor cannot distinguish "this share exists but
// isn't yours" from "this share does not exist" — the same shape as
// shareHandler's NotFound mapping.
//
// Same sentinel-error mapping as shareHandler: ErrNotFound → 404,
// ErrExpired → 410, anything else → logged + generic 500.
func (s *Server) createdHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		s.renderError(w, r, http.StatusNotFound, "error.notfound.title", "error.notfound.message")
		return
	}

	sh, err := s.share.Get(r.Context(), id)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}

	if sh.UserID != user.ID {
		// 404 (not 403) keeps the existence of someone else's share invisible.
		// A probing user with a guessed ID gets the same response shape as a
		// genuinely missing one — no leak, no oracle.
		s.renderError(w, r, http.StatusNotFound, "error.notfound.title", "error.notfound.message")
		return
	}

	s.render(w, http.StatusOK, "share_created.html", shareCreatedView{
		Lang: middleware.LangFromContext(r.Context()),
		ID:   sh.ID,
		// TrimRight normalises a trailing slash on BaseURL so an operator
		// setting BASE_URL=https://example.com/ doesn't produce a
		// double-slashed share URL. Same reasoning as the bot's
		// buildShareReply (internal/bot/handlers.go).
		ShareURL:  strings.TrimRight(s.cfg.BaseURL, "/") + "/" + sh.ID,
		Kind:      sh.Kind,
		Filename:  stringOrEmpty(sh.OriginalFilename),
		Size:      int64OrZero(sh.SizeBytes),
		ExpiresAt: sh.ExpiresAt,
	})
}

// createFileShareFromPart spools the file part to a temp file on disk so we
// can stat it for size before handing it to share.CreateFileShare. The
// storage interface (and the R2 backend in particular) requires
// ContentLength up front, and multipart.Part doesn't expose one — buffering
// to a temp file is the cheapest correct path that keeps the storage
// contract uniform across backends. The temp file lives in the OS temp
// directory and is removed regardless of success or failure.
func (s *Server) createFileShareFromPart(r *http.Request, userID int64, fields uploadFields) (*share.Share, error) {
	tmp, err := os.CreateTemp("", "yacht-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create temp upload file: %w", err)
	}
	tmpName := tmp.Name()
	// belt-and-braces: close + remove on every exit. os.File.Close is safe
	// to call twice (returns ErrClosed, ignored), and Remove on a missing
	// path is harmless after a successful unlink elsewhere.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	size, err := io.Copy(tmp, fields.File)
	if err != nil {
		return nil, fmt.Errorf("spool upload to temp: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind upload temp: %w", err)
	}

	sh, err := s.share.CreateFileShare(r.Context(), share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: fields.Filename,
		MIMEType:         fields.MIMEType,
		Size:             size,
		Content:          tmp,
		Password:         fields.Password,
		Expiry:           fields.Expiry,
	})
	if err != nil {
		return nil, fmt.Errorf("create file share: %w", err)
	}
	return sh, nil
}

// defaultExpirySeconds picks the dropdown option that matches the configured
// DefaultExpiry. Falls back to 24h (the documented service default) when the
// configured value isn't on the allowlist — a misconfigured default
// shouldn't make the form unselectable. Pure helper so the upload-form tests
// can verify the selection logic without touching templates.
func defaultExpirySeconds(d time.Duration) int64 {
	want := int64(d.Seconds())
	for _, opt := range expiryOptions {
		if opt.Seconds == want {
			return opt.Seconds
		}
	}
	return 86400
}

// uploadFieldMaxBytes caps each non-file form field at 64 KB. The four such
// fields (kind, password, expiry, text) all carry short string values; a
// 64 KB cap is generous for a paste-into-text-area share while still small
// enough that a malformed client streaming gigabytes into one field can't
// exhaust process memory. The same constant is the per-field LimitReader
// guard inside parseUploadForm and is surfaced to the upload form as the
// text-share content cap so the operator sees an actionable limit before
// they hit it.
const uploadFieldMaxBytes = 64 * 1024

// errTextTooLarge is returned (wrapped) by parseUploadForm when the text
// field overflows uploadFieldMaxBytes. Distinct from the generic field-
// overflow path so the handler can render the size-specific friendly banner
// (mentioning the text cap) instead of the catch-all "could not process"
// message — text is the only non-file field a normal operator can drive
// over the cap by hand. Match with errors.Is.
var errTextTooLarge = errors.New("upload: text content exceeds limit")

// uploadFields is the parsed projection of a POST /upload multipart body.
// File is non-nil for kind=file and the caller is expected to stream from it
// straight into share.CreateFileShare without buffering — the underlying
// part is held open by the active multipart reader, which in turn is held
// open by the request body. Filename + MIMEType are pulled from the file
// part's Content-Disposition / Content-Type headers; for kind=text both are
// empty.
type uploadFields struct {
	Kind     string
	Password string
	Expiry   time.Duration
	Text     string
	File     *multipart.Part
	Filename string
	MIMEType string
}

// allowedExpiry maps a submitted expiry value (in seconds) to a Duration if
// the value is on the form's allowlist. Returning a (Duration, ok) pair —
// rather than a sentinel error — keeps the validation surface inside
// parseUploadForm uniform: every "field rejected" path produces the same
// shape of wrapped error there.
func allowedExpiry(seconds int64) (time.Duration, bool) {
	for _, opt := range expiryOptions {
		if opt.Seconds == seconds {
			return time.Duration(seconds) * time.Second, true
		}
	}
	return 0, false
}

// parseUploadForm streams the POST /upload multipart body part-by-part and
// returns the parsed metadata + a still-open file part for the caller to
// stream into share.CreateFileShare. Splitting parsing out of the handler
// (a) keeps the validation rules unit-testable without spinning a fake
// share.Service and (b) draws the line between "this request is shaped
// correctly" and "this request creates a share" so the handler's success
// path stays a straight read.
//
// The body is wrapped with http.MaxBytesReader at maxBytes + 64 KB headroom
// so an oversized upload fails on read with *http.MaxBytesError instead of
// pulling unbounded bytes through the parser. The 64 KB headroom covers the
// non-file form fields plus multipart boundaries — the share's payload
// itself counts against maxBytes proper.
//
// Field-order assumption (Phase 10 plan, decision #2): the file part is the
// last part in the stream. The form template enforces this in HTML; here we
// stop iterating the moment we see the file part and hand the still-open
// reader to the caller. Anything that arrives after a file part is silently
// ignored — a deliberate choice so a buggy client that re-orders fields
// fails closed (missing kind/expiry → validation error) rather than
// half-succeeding with whichever fields landed before the file.
//
// Empty file inputs (no file selected — browsers still send a part with
// empty filename) are skipped: parsing continues so the trailing fields
// after an empty file input still land. This matters when a user with the
// kind=text radio active still has the file <input> element inside the form
// — the browser submits both, and we want validation to land on
// kind/text checks rather than tripping on an empty file part.
func parseUploadForm(r *http.Request, maxBytes int64) (uploadFields, error) {
	// MaxBytesReader's first arg is the ResponseWriter it would otherwise
	// add a Connection: close header to on overflow; nil is safe here and
	// keeps the helper independent of the response writer so tests can
	// drive it from a bare *http.Request.
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes+uploadFieldMaxBytes)

	mr, err := r.MultipartReader()
	if err != nil {
		return uploadFields{}, fmt.Errorf("parse upload: read multipart: %w", err)
	}

	var fields uploadFields
	expirySet := false

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return uploadFields{}, fmt.Errorf("parse upload: next part: %w", err)
		}

		name := part.FormName()
		if name == "file" {
			// File part — capture filename/MIME and hand the reader to the
			// caller. ReplaceAll normalises Windows-style backslash paths
			// (e.g. "C:\fakepath\report.pdf" from older Edge / certain
			// mobile webviews) so filepath.Base — which only treats "/" as
			// a separator on Linux/macOS — still strips the bogus prefix
			// instead of letting it land in OriginalFilename. An empty
			// result (no file selected — common when the kind=text radio
			// is active but the file <input> still ships with the form)
			// is treated as "no file" so trailing fields after it still parse.
			filename := filepath.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
			if filename == "" || filename == "." || filename == "/" {
				_ = part.Close()
				continue
			}
			mime := part.Header.Get("Content-Type")
			if mime == "" {
				// Browsers usually populate Content-Type from the OS MIME
				// guess, but a missing header is legal per RFC 7578.
				// application/octet-stream is the spec's documented default
				// and matches what http.DetectContentType returns for a
				// stream with no recognizable signature.
				mime = "application/octet-stream"
			}
			fields.File = part
			fields.Filename = filename
			fields.MIMEType = mime
			break
		}

		// Non-file field. We read up to uploadFieldMaxBytes+1 so an oversized
		// field is reliably detected: io.LimitReader returns io.EOF when the
		// cap is hit and io.ReadAll then returns no error, so reading exactly
		// uploadFieldMaxBytes would silently truncate. Reading one extra byte
		// lets us notice the overflow and reject — otherwise a 200 KB pasted
		// text would land as the first 64 KB with no warning.
		buf, err := io.ReadAll(io.LimitReader(part, uploadFieldMaxBytes+1))
		_ = part.Close()
		if err != nil {
			return uploadFields{}, fmt.Errorf("parse upload: read field %q: %w", name, err)
		}
		if int64(len(buf)) > uploadFieldMaxBytes {
			// Tag the text-field overflow with errTextTooLarge so the handler
			// can render the size-aware banner. Other field overflows
			// (kind/password/expiry) still surface as the generic parse error
			// — those fields aren't operator-typed at scale, so an oversize
			// is a malformed-client signal worth keeping generic.
			if name == "text" {
				return uploadFields{}, fmt.Errorf("parse upload: field %q exceeds %d-byte cap: %w", name, uploadFieldMaxBytes, errTextTooLarge)
			}
			return uploadFields{}, fmt.Errorf("parse upload: field %q exceeds %d-byte cap", name, uploadFieldMaxBytes)
		}

		switch name {
		case "kind":
			fields.Kind = string(buf)
		case "password":
			fields.Password = string(buf)
		case "expiry":
			raw := strings.TrimSpace(string(buf))
			secs, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return uploadFields{}, fmt.Errorf("parse upload: invalid expiry %q: %w", raw, err)
			}
			d, ok := allowedExpiry(secs)
			if !ok {
				return uploadFields{}, fmt.Errorf("parse upload: expiry %d not allowed", secs)
			}
			fields.Expiry = d
			expirySet = true
		case "text":
			fields.Text = string(buf)
		}
		// Unknown fields are silently dropped: a future template addition
		// that lands ahead of a corresponding server change shouldn't 400.
	}

	if fields.Kind != share.KindFile && fields.Kind != share.KindText {
		return uploadFields{}, fmt.Errorf("parse upload: invalid kind %q", fields.Kind)
	}
	if !expirySet {
		return uploadFields{}, fmt.Errorf("parse upload: expiry missing")
	}

	switch fields.Kind {
	case share.KindText:
		if fields.Text == "" {
			return uploadFields{}, fmt.Errorf("parse upload: text content is empty")
		}
		if fields.File != nil {
			return uploadFields{}, fmt.Errorf("parse upload: text kind must not include a file part")
		}
	case share.KindFile:
		if fields.File == nil {
			return uploadFields{}, fmt.Errorf("parse upload: file kind requires a file part")
		}
	}

	return fields, nil
}

// loginHandler serves GET /login: the login page with the Telegram Login
// Widget plus a fallback block pointing the user at the /weblogin bot
// command for networks where the widget is blocked. An optional ?error=
// query param surfaces a banner above the widget — only codes listed in
// loginErrorMessages render anything; arbitrary values are dropped on the
// floor so the page never echoes untrusted text.
func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	lang := middleware.LangFromContext(r.Context())
	errMsg := ""
	if key, ok := loginErrorMessages[r.URL.Query().Get("error")]; ok {
		errMsg = i18n.T(lang, key)
	}
	s.render(w, http.StatusOK, "login.html", loginView{
		Lang:        lang,
		BotUsername: s.cfg.TelegramBotUsername,
		Error:       errMsg,
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
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
		return
	}

	sessionID, err := auth.CreateSession(r.Context(), s.db, user.ID, s.authTelegram.Name(), s.cfg.SessionLifetime)
	if err != nil {
		s.logger.Error("create session (telegram widget)", "user_id", user.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
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
			s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
		}
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.logger.Error("begin tx (bot token)", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
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
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
		return
	}

	sessionID, err := auth.CreateSessionTx(r.Context(), tx, user.ID, s.authBotToken.Name(), s.cfg.SessionLifetime)
	if err != nil {
		s.logger.Error("create session (bot token)", "user_id", user.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
		return
	}

	if err := tx.Commit(); err != nil {
		s.logger.Error("commit (bot token)", "user_id", user.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
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

// langHandler serves GET /lang/{code}: the explicit language switcher
// endpoint. Anonymous-friendly so a first-time visitor can flip languages
// before logging in; authenticated visitors additionally have their pick
// mirrored into users.lang so the preference survives a cookie clear.
//
// Failure modes:
//
//   - Unknown code → 400 via renderError. Never trust path-supplied lang
//     values: an attacker who plants a yacht_lang cookie with a junk value
//     would otherwise leak it through the middleware's IsSupported guard
//     ignoring it; rejecting at the handler keeps junk out of the cookie
//     jar entirely.
//   - DB write failure on the users.lang update → logged at WARN and
//     swallowed. The visitor's primary intent (cookie set + redirect) has
//     already succeeded; surfacing a 500 here would make a transient DB
//     hiccup look like a broken language switcher.
//
// Open-redirect guard: the redirect target is computed from r.Referer()
// only when the parsed URL's host matches r.Host (or is absent — a bare
// path Referer). Any cross-origin or unparseable Referer collapses to "/"
// so an attacker who lures a logged-in user into clicking
// /lang/ru?... from an external page cannot use the lang endpoint as a
// generic open-redirect.
func (s *Server) langHandler(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !i18n.IsSupported(code) {
		s.renderError(w, r, http.StatusBadRequest, "error.badrequest.title", "error.badrequest.unsupportedlang")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     langCookieName,
		Value:    code,
		Path:     "/",
		MaxAge:   int(langCookieMaxAge.Seconds()),
		HttpOnly: true,
		// Lax (not Strict) so the cookie survives a top-level navigation
		// from another origin — clicking a /lang/ru link in an email or
		// chat must still set the cookie. Strict would drop it for those
		// flows and produce a confusing "the switcher does nothing"
		// experience.
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
	})

	if user := s.userFromSessionCookie(r); user != nil {
		// Best-effort persistence: a DB hiccup must not block the
		// redirect. The cookie is already set, so the visitor's intent
		// has been honoured for the current browser; the persisted
		// preference catches up the next time they log in or roll the
		// cookie over.
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE users SET lang = ? WHERE id = ?`, code, user.ID); err != nil {
			s.logger.Warn("update users.lang", "user_id", user.ID, "err", err)
		}
	}

	http.Redirect(w, r, safeRedirectFromReferer(r), http.StatusSeeOther)
}

// userFromSessionCookie best-effort resolves the request's session cookie
// to a hydrated *auth.User. Returns nil for any failure (missing cookie,
// expired session, stale row, DB fault) — every nil-return path is
// equivalent from langHandler's perspective: "no user to update, just set
// the cookie and continue". Pulled out as a method so future
// anonymous-friendly routes that want the same best-effort lookup can
// reuse the contract without re-implementing the cookie/parse/lookup
// dance.
func (s *Server) userFromSessionCookie(r *http.Request) *auth.User {
	if s.cfg == nil || s.cfg.SessionCookieName == "" {
		return nil
	}
	c, err := r.Cookie(s.cfg.SessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	user, err := auth.GetSession(r.Context(), s.db, c.Value)
	if err != nil {
		return nil
	}
	return user
}

// safeRedirectFromReferer returns a path-only redirect target derived from
// the Referer header, or "/" when the referer is missing, unparseable, or
// cross-origin. The open-redirect guard compares the parsed URL's host
// against r.Host — an external host (or a Referer we cannot parse at all)
// collapses to "/" so the lang endpoint cannot be used as a generic
// open-redirect.
//
// The returned target is path+query only (never scheme+host) so even a
// same-origin Referer never leaks into the Location header as an absolute
// URL — keeping things relative also makes the response survive a reverse
// proxy that rewrites scheme/host.
func safeRedirectFromReferer(r *http.Request) string {
	ref := r.Referer()
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	if u.Host != "" && u.Host != r.Host {
		return "/"
	}
	target := u.Path
	if target == "" {
		return "/"
	}
	// Defense-in-depth: a Referer with no host (e.g. "//evil.com/x" or
	// "/\evil.com") parses with empty Host and a path that some browsers
	// historically resolved as a network-relative URL. Reject any path that
	// would let the Location header escape the current origin.
	if strings.HasPrefix(target, "//") || strings.HasPrefix(target, `/\`) {
		return "/"
	}
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
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
		s.renderError(w, r, http.StatusNotFound, "error.notfound.title", "error.notfound.message")
		return
	}

	sh, err := s.share.Get(r.Context(), id)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}

	if sh.PasswordHash != nil && !s.hasShareCookie(r, sh) {
		s.render(w, http.StatusUnauthorized, "password.html", passwordPromptView{
			Lang: middleware.LangFromContext(r.Context()),
			ID:   id,
		})
		return
	}

	switch sh.Kind {
	case share.KindFile:
		s.render(w, http.StatusOK, "share_file.html", fileShareView{
			Lang:      middleware.LangFromContext(r.Context()),
			ID:        sh.ID,
			Filename:  stringOrEmpty(sh.OriginalFilename),
			Size:      int64OrZero(sh.SizeBytes),
			ExpiresAt: sh.ExpiresAt,
		})
	case share.KindText:
		s.render(w, http.StatusOK, "share_text.html", textShareView{
			Lang:      middleware.LangFromContext(r.Context()),
			ID:        sh.ID,
			Content:   stringOrEmpty(sh.TextContent),
			ExpiresAt: sh.ExpiresAt,
		})
	default:
		// a row with an unknown kind means the DB was written by code we
		// don't own or someone edited rows by hand; log and fail loud so the
		// operator investigates rather than silently rendering an empty page.
		s.logger.Error("unknown share kind", "id", id, "kind", sh.Kind)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.share.unavailable")
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
		s.renderError(w, r, http.StatusNotFound, "error.notfound.title", "error.notfound.message")
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
		s.renderError(w, r, http.StatusBadRequest, "error.badrequest.title", "error.badrequest.share_notprotected")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, passwordFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		s.logger.Error("password form parse failed", "id", id, "err", err)
		s.renderError(w, r, http.StatusBadRequest, "error.badrequest.title", "error.badrequest.form_read")
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
		lang := middleware.LangFromContext(r.Context())
		s.render(w, http.StatusUnauthorized, "password.html", passwordPromptView{
			Lang:  lang,
			ID:    id,
			Error: i18n.T(lang, "error.password.incorrect"),
		})
		return
	default:
		s.logger.Error("verify password failed", "id", id, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
		return
	}
}

// renderShareError maps share.Service sentinel errors to HTTP status codes
// and the error.html template. Any unrecognized error is logged and surfaced
// as a generic 500 so internal detail never leaks into the response body.
func (s *Server) renderShareError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, share.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "error.notfound.title", "error.notfound.message")
	case errors.Is(err, share.ErrExpired):
		s.renderError(w, r, http.StatusGone, "error.gone.title", "error.gone.share_expired")
	default:
		s.logger.Error("share lookup failed", "method", r.Method, "path", r.URL.Path, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
	}
}

// renderError is the single entry point that writes the error.html template.
// titleKey and messageKey are i18n bundle keys; the resolved language comes
// off the request context (set by the lang middleware). Routing every error
// page through one helper means the bundle lookup happens in exactly one
// place — call sites pass keys, never raw English — so a future bundle
// rename only touches the call sites, not the render path.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, titleKey, messageKey string) {
	lang := middleware.LangFromContext(r.Context())
	s.render(w, status, "error.html", errorView{
		Lang:    lang,
		Title:   i18n.T(lang, titleKey),
		Message: i18n.T(lang, messageKey),
	})
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
		s.renderError(w, r, http.StatusNotFound, "error.notfound.title", "error.notfound.message")
		return
	}

	sh, err := s.share.Get(r.Context(), id)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}

	if sh.PasswordHash != nil && !s.hasShareCookie(r, sh) {
		s.render(w, http.StatusUnauthorized, "password.html", passwordPromptView{
			Lang: middleware.LangFromContext(r.Context()),
			ID:   id,
		})
		return
	}

	switch sh.Kind {
	case share.KindFile:
		s.streamFileShare(w, r, sh)
	case share.KindText:
		s.streamTextShare(w, r, sh)
	default:
		s.logger.Error("unknown share kind", "id", id, "kind", sh.Kind)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.share.unavailable")
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
			s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.storage.missing")
			return
		}
		s.logger.Error("open content failed", "id", sh.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "error.internal.title", "error.internal.message")
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
