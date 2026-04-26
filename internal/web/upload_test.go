package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/storage/local"
	"golang.org/x/crypto/bcrypt"
)

// newUploadTestServer builds a Server backed by a real *sql.DB plus a real
// local-storage-backed share.Service so the RequireAuth gate the upload
// routes ride behind can resolve session cookies AND uploadSubmitHandler can
// actually persist shares. DefaultExpiry is fixed at 24h so the form's
// pre-selected option is the canonical one; MaxUploadBytes is small enough
// that an oversized-body regression test would catch a missing
// MaxBytesReader without churning real megabytes through the recorder.
func newUploadTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	srv, _, handle := newUploadTestServerWithService(t, 1024*1024)
	return srv, handle
}

// newUploadTestServerWithService is the full-stack flavour of
// newUploadTestServer: same wiring, plus exposes the *share.Service so
// submit-path tests can create or read shares directly. maxUpload lets a
// caller dial the size cap down for the oversized-body test without
// affecting the rest of the suite.
func newUploadTestServerWithService(t *testing.T, maxUpload int64) (*Server, *share.Service, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "upload.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	shared := &config.Shared{
		DefaultExpiry:  24 * time.Hour,
		MaxUploadBytes: maxUpload,
	}
	svc := share.New(handle, backend, shared)

	cfg := &config.Web{
		Shared:            shared,
		SessionCookieName: "yacht_session",
		SessionLifetime:   30 * 24 * time.Hour,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := New(cfg, handle, svc, nil, nil, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, svc, handle
}

// insertUploadTestAdmin inserts an admin users row so CreateSession's
// downstream RequireAuth lookup (which enforces is_admin=1) can succeed.
// telegram_id uses wall-clock nanos to avoid the UNIQUE constraint colliding
// across tests in the same process.
func insertUploadTestAdmin(t *testing.T, handle *sql.DB) int64 {
	t.Helper()
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, 1, strftime('%s','now'))`,
		time.Now().UnixNano(), "uploader", "Uploader",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// uploadTestSession mints a real session row for the user and returns the
// cookie value. Tests that want to exercise an authenticated request thread
// it through req.AddCookie so RequireAuth resolves it the same way the
// production middleware would.
func uploadTestSession(t *testing.T, handle *sql.DB, userID int64) string {
	t.Helper()
	sessionID, err := auth.CreateSession(
		context.Background(), handle, userID, "test", 30*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sessionID
}

// TestUploadForm_RequiresAuth proves GET /upload is wired behind the
// RequireAuth middleware: a request without a session cookie must redirect
// to /login (303) rather than render the form. Without this guard, a
// routing change that drops the gate would silently leak the upload form to
// the public.
func TestUploadForm_RequiresAuth(t *testing.T) {
	srv, _ := newUploadTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}
}

// TestUploadForm_RendersForm exercises the happy path: with a valid session,
// GET /upload renders the form with the kind radio, password input, expiry
// select carrying all six allowlist options, the text area, and the file
// input. Substring matching keeps Phase 14 styling tweaks from breaking the
// test, while still pinning the structural pieces handler logic relies on.
func TestUploadForm_RendersForm(t *testing.T) {
	srv, handle := newUploadTestServer(t)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`method="post"`,
		`action="/upload"`,
		`enctype="multipart/form-data"`,
		`name="kind"`,
		`value="file"`,
		`value="text"`,
		`name="password"`,
		`name="expiry"`,
		`name="text"`,
		`name="file"`,
		`type="file"`,
		`<textarea`,
		`<select`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	for _, secs := range []string{"3600", "21600", "86400", "259200", "604800", "2592000"} {
		if !strings.Contains(body, `value="`+secs+`"`) {
			t.Errorf("body missing expiry option value=%q; got:\n%s", secs, body)
		}
	}
	// the 24h option must be pre-selected against DefaultExpiry=24h.
	if !strings.Contains(body, `value="86400" selected`) {
		t.Errorf("body missing pre-selected 24h option; got:\n%s", body)
	}
	// both size hints must render so the operator sees the right cap before
	// they hit it — text rides the per-field cap, files ride MaxUploadBytes.
	for _, want := range []string{"Maximum file size:", "Maximum text size:"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing size hint %q; got:\n%s", want, body)
		}
	}
}

// TestUploadForm_FieldOrder pins the load-bearing order from decision #2:
// non-file fields (kind, password, expiry, text) must precede the file
// input in the rendered HTML so they arrive first in the multipart stream.
// A future template tweak that re-orders these would silently break the
// streaming POST handler, so the regression guard lives here.
func TestUploadForm_FieldOrder(t *testing.T) {
	srv, handle := newUploadTestServer(t)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	body := rec.Body.String()
	order := []string{`name="kind"`, `name="password"`, `name="expiry"`, `name="text"`, `name="file"`}
	pos := -1
	for _, marker := range order {
		next := strings.Index(body, marker)
		if next < 0 {
			t.Fatalf("body missing %q; got:\n%s", marker, body)
		}
		if next <= pos {
			t.Errorf("field %q out of order: index %d not after previous %d", marker, next, pos)
		}
		pos = next
	}
}

// uploadFileSpec carries the bytes-on-the-wire shape of a multipart file
// part for buildMultipartBody. Filename empty means "do not include a file
// part at all"; an empty filename with a present spec models the
// browser-sent "no file selected" case (file part with empty
// Content-Disposition filename).
type uploadFileSpec struct {
	Filename    string
	ContentType string
	Content     []byte
}

// uploadFormSpec describes the canonical fields the upload form posts. Each
// *string field maps to "include this field with the given value" when
// non-nil and "omit this field entirely" when nil — distinguishing an empty
// password (which still posts) from a missing one (which does not). Field
// order in the encoded body matches the form template (kind → password →
// expiry → text → file) so parseUploadForm sees the same stream the browser
// produces.
type uploadFormSpec struct {
	Kind     *string
	Password *string
	Expiry   *string
	Text     *string
	File     *uploadFileSpec
}

// strPtr is a tiny helper so test cases can write Kind: strPtr("text")
// inline without sprinkling per-test helper variables. Pure convenience —
// the type is *string, not anything fancy.
func strPtr(s string) *string { return &s }

// buildMultipartBody encodes the canonical upload form into a multipart
// body. Returns the raw body bytes plus the matching Content-Type header
// (which carries the boundary). Tests feed the body to httptest.NewRequest
// and set the returned content type so r.MultipartReader inside
// parseUploadForm sees the same stream a browser would post.
func buildMultipartBody(t *testing.T, spec uploadFormSpec) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if spec.Kind != nil {
		if err := mw.WriteField("kind", *spec.Kind); err != nil {
			t.Fatalf("write kind: %v", err)
		}
	}
	if spec.Password != nil {
		if err := mw.WriteField("password", *spec.Password); err != nil {
			t.Fatalf("write password: %v", err)
		}
	}
	if spec.Expiry != nil {
		if err := mw.WriteField("expiry", *spec.Expiry); err != nil {
			t.Fatalf("write expiry: %v", err)
		}
	}
	if spec.Text != nil {
		if err := mw.WriteField("text", *spec.Text); err != nil {
			t.Fatalf("write text: %v", err)
		}
	}
	if spec.File != nil {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, spec.File.Filename))
		if spec.File.ContentType != "" {
			h.Set("Content-Type", spec.File.ContentType)
		}
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(spec.File.Content); err != nil {
			t.Fatalf("write file content: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// newUploadParseRequest wraps buildMultipartBody into an *http.Request ready
// for parseUploadForm: POST /upload with the encoded body and the matching
// multipart Content-Type. Centralizing the wiring keeps the parser tests
// focused on the input/output shape rather than HTTP plumbing.
func newUploadParseRequest(t *testing.T, spec uploadFormSpec) *http.Request {
	t.Helper()
	body, ct := buildMultipartBody(t, spec)
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", ct)
	return req
}

// TestParseUploadForm_TextHappyPath: a well-formed kind=text submission
// returns the parsed fields with Expiry mapped from seconds → Duration and
// no file reader attached. Pins the canonical success case so a refactor
// that breaks field plumbing surfaces immediately.
func TestParseUploadForm_TextHappyPath(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:     strPtr("text"),
		Password: strPtr(""),
		Expiry:   strPtr("86400"),
		Text:     strPtr("hello"),
	})

	fields, err := parseUploadForm(req, 1024*1024)
	if err != nil {
		t.Fatalf("parseUploadForm: %v", err)
	}
	if fields.Kind != "text" {
		t.Errorf("Kind: want %q, got %q", "text", fields.Kind)
	}
	if fields.Password != "" {
		t.Errorf("Password: want %q, got %q", "", fields.Password)
	}
	if fields.Expiry != 24*time.Hour {
		t.Errorf("Expiry: want %v, got %v", 24*time.Hour, fields.Expiry)
	}
	if fields.Text != "hello" {
		t.Errorf("Text: want %q, got %q", "hello", fields.Text)
	}
	if fields.File != nil {
		t.Errorf("File: want nil for kind=text, got %+v", fields.File)
	}
	if fields.Filename != "" {
		t.Errorf("Filename: want empty for kind=text, got %q", fields.Filename)
	}
}

// TestParseUploadForm_FileHappyPath: a well-formed kind=file submission
// returns a non-nil File reader along with filename + MIME type pulled from
// the part headers, and reading the reader yields the exact bytes the
// client sent. The reader must still be open at this point — the handler
// streams from it straight into share.CreateFileShare.
func TestParseUploadForm_FileHappyPath(t *testing.T) {
	payload := []byte("file payload bytes")
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:     strPtr("file"),
		Password: strPtr("hunter2"),
		Expiry:   strPtr("3600"),
		Text:     strPtr(""),
		File: &uploadFileSpec{
			Filename:    "report.pdf",
			ContentType: "application/pdf",
			Content:     payload,
		},
	})

	fields, err := parseUploadForm(req, 1024*1024)
	if err != nil {
		t.Fatalf("parseUploadForm: %v", err)
	}
	if fields.Kind != "file" {
		t.Errorf("Kind: want %q, got %q", "file", fields.Kind)
	}
	if fields.Password != "hunter2" {
		t.Errorf("Password: want %q, got %q", "hunter2", fields.Password)
	}
	if fields.Expiry != time.Hour {
		t.Errorf("Expiry: want %v, got %v", time.Hour, fields.Expiry)
	}
	if fields.Filename != "report.pdf" {
		t.Errorf("Filename: want %q, got %q", "report.pdf", fields.Filename)
	}
	if fields.MIMEType != "application/pdf" {
		t.Errorf("MIMEType: want %q, got %q", "application/pdf", fields.MIMEType)
	}
	if fields.File == nil {
		t.Fatalf("File: want non-nil reader, got nil")
	}
	got, err := io.ReadAll(fields.File)
	if err != nil {
		t.Fatalf("read file part: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("file bytes: want %q, got %q", payload, got)
	}
}

// TestParseUploadForm_FileMissingMIMEDefaults: when a client omits the
// per-part Content-Type header (legal per RFC 7578), the parser falls back
// to application/octet-stream so downstream storage code never sees an
// empty MIME type.
func TestParseUploadForm_FileMissingMIMEDefaults(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("file"),
		Expiry: strPtr("86400"),
		File: &uploadFileSpec{
			Filename: "noctype.bin",
			Content:  []byte("x"),
		},
	})

	fields, err := parseUploadForm(req, 1024*1024)
	if err != nil {
		t.Fatalf("parseUploadForm: %v", err)
	}
	if fields.MIMEType != "application/octet-stream" {
		t.Errorf("MIMEType fallback: want %q, got %q", "application/octet-stream", fields.MIMEType)
	}
}

// TestParseUploadForm_FileStripsDirectoryPrefix: filepath.Base must strip a
// leading directory component from a browser-supplied filename. Some
// clients (older WebKit on certain platforms) send the full path. The
// share is keyed on the basename, so leaking the directory would be a
// pointless info disclosure on top of being structurally wrong.
func TestParseUploadForm_FileStripsDirectoryPrefix(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("file"),
		Expiry: strPtr("86400"),
		File: &uploadFileSpec{
			Filename:    "/Users/alice/Downloads/report.pdf",
			ContentType: "application/pdf",
			Content:     []byte("x"),
		},
	})

	fields, err := parseUploadForm(req, 1024*1024)
	if err != nil {
		t.Fatalf("parseUploadForm: %v", err)
	}
	if fields.Filename != "report.pdf" {
		t.Errorf("Filename: want basename %q, got %q", "report.pdf", fields.Filename)
	}
}

// TestParseUploadForm_FileStripsWindowsBackslashPath: a Windows-style path
// (e.g. "C:\fakepath\report.pdf" — what older Edge / certain mobile webviews
// still send) must be stripped to its basename. filepath.Base on Linux/macOS
// only treats "/" as a separator, so without the explicit backslash
// normalisation in the parser the bogus drive prefix would land in
// OriginalFilename and leak into the download Content-Disposition.
func TestParseUploadForm_FileStripsWindowsBackslashPath(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("file"),
		Expiry: strPtr("86400"),
		File: &uploadFileSpec{
			Filename:    `C:\fakepath\report.pdf`,
			ContentType: "application/pdf",
			Content:     []byte("x"),
		},
	})

	fields, err := parseUploadForm(req, 1024*1024)
	if err != nil {
		t.Fatalf("parseUploadForm: %v", err)
	}
	if fields.Filename != "report.pdf" {
		t.Errorf("Filename: want basename %q, got %q", "report.pdf", fields.Filename)
	}
}

// TestParseUploadForm_FieldOverflowRejected: a non-file field whose value
// exceeds the 64 KB per-field cap must reject loud rather than silently
// truncate. io.LimitReader returns io.EOF at the cap with no error from
// io.ReadAll, so a naive read would have silently kept only the first 64 KB
// of a 200 KB pasted text — the parser reads cap+1 and rejects on overflow
// to surface the cap mismatch instead of letting it through. For the text
// field specifically the error must wrap errTextTooLarge so the handler can
// surface a size-aware banner instead of the catch-all parse message.
func TestParseUploadForm_FieldOverflowRejected(t *testing.T) {
	bigText := strings.Repeat("a", uploadFieldMaxBytes+1)

	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr(bigText),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error on oversized field, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error: want 'exceeds' substring, got %v", err)
	}
	if !errors.Is(err, errTextTooLarge) {
		t.Errorf("error: want errors.Is errTextTooLarge, got %v", err)
	}
}

// TestParseUploadForm_BadKind: a kind value outside the {file, text}
// allowlist must surface as an "invalid kind" error.
func TestParseUploadForm_BadKind(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("junk"),
		Expiry: strPtr("86400"),
		Text:   strPtr("hello"),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Errorf("error: want 'invalid kind' substring, got %v", err)
	}
}

// TestParseUploadForm_KindMissing: an entirely missing kind field must also
// reject as "invalid kind" — distinct from the rest of the validation
// chain so the user gets a single clear error rather than a downstream
// "expiry missing" that would mislead them about the actual problem.
func TestParseUploadForm_KindMissing(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Expiry: strPtr("86400"),
		Text:   strPtr("hello"),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Errorf("error: want 'invalid kind' substring, got %v", err)
	}
}

// TestParseUploadForm_TextEmptyContent: kind=text with an empty text field
// must reject — a text share with no content is meaningless and would
// fail at share.CreateTextShare anyway.
func TestParseUploadForm_TextEmptyContent(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr(""),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "text content is empty") {
		t.Errorf("error: want 'text content is empty' substring, got %v", err)
	}
}

// TestParseUploadForm_TextWithFileRejected: kind=text with a real file part
// attached must reject. The form's JS toggle hides the file input when
// kind=text is selected, so a populated file part here means the client
// bypassed the UI — surface it rather than silently dropping the file.
func TestParseUploadForm_TextWithFileRejected(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr("hello"),
		File: &uploadFileSpec{
			Filename:    "extra.bin",
			ContentType: "application/octet-stream",
			Content:     []byte("uninvited"),
		},
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "must not include a file part") {
		t.Errorf("error: want 'must not include a file part' substring, got %v", err)
	}
}

// TestParseUploadForm_TextWithEmptyFilePart: a browser submitting kind=text
// while the file <input> sits empty in the form will still ship a file
// part with an empty Content-Disposition filename. That case must NOT
// reject — it's the normal mode of operation when the user toggled to text
// without removing the file element.
func TestParseUploadForm_TextWithEmptyFilePart(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr("hello"),
		File: &uploadFileSpec{
			Filename: "",
			Content:  nil,
		},
	})

	fields, err := parseUploadForm(req, 1024*1024)
	if err != nil {
		t.Fatalf("parseUploadForm: want no error, got %v", err)
	}
	if fields.File != nil {
		t.Errorf("File: want nil for empty file part, got %+v", fields.File)
	}
	if fields.Text != "hello" {
		t.Errorf("Text: want %q, got %q", "hello", fields.Text)
	}
}

// TestParseUploadForm_FileMissing: kind=file with no file part at all must
// reject — the operator picked the file mode but submitted nothing.
func TestParseUploadForm_FileMissing(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("file"),
		Expiry: strPtr("86400"),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "requires a file part") {
		t.Errorf("error: want 'requires a file part' substring, got %v", err)
	}
}

// TestParseUploadForm_FileEmptyFilenameRejected: kind=file with a file part
// present but no filename selected (empty Content-Disposition filename) is
// the "user clicked submit before choosing a file" case — surface as an
// error rather than coercing to a default name that would silently
// succeed.
func TestParseUploadForm_FileEmptyFilenameRejected(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("file"),
		Expiry: strPtr("86400"),
		File: &uploadFileSpec{
			Filename: "",
			Content:  nil,
		},
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "requires a file part") {
		t.Errorf("error: want 'requires a file part' substring, got %v", err)
	}
}

// TestParseUploadForm_ExpiryNotAllowed: an expiry value that parses as a
// number but isn't on the allowlist must reject. Anything outside the six
// dropdown options is server-policy violation, not a parsing error.
func TestParseUploadForm_ExpiryNotAllowed(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("99999"),
		Text:   strPtr("hello"),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error: want 'not allowed' substring, got %v", err)
	}
}

// TestParseUploadForm_ExpiryNotInteger: a non-integer expiry value must be
// rejected by the strconv.ParseInt step before the allowlist check ever
// runs — keeps the malformed-vs-disallowed distinction visible in errors.
func TestParseUploadForm_ExpiryNotInteger(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("forever"),
		Text:   strPtr("hello"),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid expiry") {
		t.Errorf("error: want 'invalid expiry' substring, got %v", err)
	}
}

// TestParseUploadForm_ExpiryMissing: omitting the expiry field entirely
// must reject — an unset expiry would default to zero, which is neither a
// valid duration nor on the allowlist.
func TestParseUploadForm_ExpiryMissing(t *testing.T) {
	req := newUploadParseRequest(t, uploadFormSpec{
		Kind: strPtr("text"),
		Text: strPtr("hello"),
	})

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "expiry missing") {
		t.Errorf("error: want 'expiry missing' substring, got %v", err)
	}
}

// TestParseUploadForm_TooLarge: a body larger than maxBytes + 64 KB
// headroom must trip MaxBytesReader on read and surface the typed
// *http.MaxBytesError so the handler can map it to a friendly 413.
// Constructing the test with a tiny maxBytes (1 byte) plus a text field
// well over the 64 KB headroom keeps the payload size manageable.
func TestParseUploadForm_TooLarge(t *testing.T) {
	// Text payload sized to comfortably exceed maxBytes + 64 KB headroom.
	// 128 KB of "a" is enough that even with all other multipart overhead
	// counted in, MaxBytesReader trips before the parser finishes draining
	// the field.
	bigText := strings.Repeat("a", 128*1024)

	req := newUploadParseRequest(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr(bigText),
	})

	_, err := parseUploadForm(req, 1)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Errorf("error: want *http.MaxBytesError in chain, got %T: %v", err, err)
	}
}

// TestParseUploadForm_NotMultipart: a request without a multipart
// Content-Type must surface r.MultipartReader's error wrapped — the
// handler maps this to a 400 in Task 3.
func TestParseUploadForm_NotMultipart(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	_, err := parseUploadForm(req, 1024*1024)
	if err == nil {
		t.Fatalf("parseUploadForm: want error, got nil")
	}
	if !strings.Contains(err.Error(), "read multipart") {
		t.Errorf("error: want 'read multipart' substring, got %v", err)
	}
}

// TestDefaultExpirySeconds covers the unit-level fallback: an unrecognized
// configured DefaultExpiry must collapse to 86400 (24h) so the dropdown
// always has a selected option. Keeps the helper honest independently of
// the template so a future refactor can move the call site without losing
// the guarantee.
func TestDefaultExpirySeconds(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want int64
	}{
		{"matches 1h option", 1 * time.Hour, 3600},
		{"matches 24h option", 24 * time.Hour, 86400},
		{"matches 30d option", 30 * 24 * time.Hour, 2592000},
		{"unmatched falls back to 24h", 5 * time.Minute, 86400},
		{"zero falls back to 24h", 0, 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultExpirySeconds(tc.in); got != tc.want {
				t.Errorf("defaultExpirySeconds(%v): want %d, got %d", tc.in, tc.want, got)
			}
		})
	}
}

// newAuthedUploadRequest builds a POST /upload request carrying a multipart
// body and a real session cookie, the shape uploadSubmitHandler tests use
// to drive the full RequireAuth → handler flow.
func newAuthedUploadRequest(t *testing.T, sessionID string, spec uploadFormSpec) *http.Request {
	t.Helper()
	body, ct := buildMultipartBody(t, spec)
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(&http.Cookie{Name: "yacht_session", Value: sessionID})
	return req
}

// TestUploadSubmit_RequiresAuth: POST /upload without a session cookie must
// 303-redirect to /login, with no share row created. RequireAuth is the only
// gate keeping unauthenticated visitors from minting shares, so a routing
// regression that drops it would silently open the upload path to the world.
func TestUploadSubmit_RequiresAuth(t *testing.T) {
	srv, _, handle := newUploadTestServerWithService(t, 1024*1024)

	body, ct := buildMultipartBody(t, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr("hello"),
	})
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("location: want %q, got %q", "/login", loc)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares`).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 0 {
		t.Errorf("shares table: want 0 rows after gated POST, got %d", count)
	}
}

// TestUploadSubmit_TextHappyPath: a well-formed kind=text submission lands
// a text share row with the submitted content, no password hash, and
// 303-redirects to /shares/{id}/created. Pinning every load-bearing piece
// means a refactor that breaks any one of them surfaces immediately.
func TestUploadSubmit_TextHappyPath(t *testing.T) {
	srv, svc, handle := newUploadTestServerWithService(t, 1024*1024)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	req := newAuthedUploadRequest(t, sessionID, uploadFormSpec{
		Kind:     strPtr("text"),
		Password: strPtr(""),
		Expiry:   strPtr("86400"),
		Text:     strPtr("a secret memo"),
	})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/shares/") || !strings.HasSuffix(loc, "/created") {
		t.Fatalf("location: want /shares/{id}/created, got %q", loc)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(loc, "/shares/"), "/created")

	sh, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %q: %v", id, err)
	}
	if sh.Kind != "text" {
		t.Errorf("Kind: want %q, got %q", "text", sh.Kind)
	}
	if sh.UserID != userID {
		t.Errorf("UserID: want %d, got %d", userID, sh.UserID)
	}
	if sh.TextContent == nil || *sh.TextContent != "a secret memo" {
		t.Errorf("TextContent: want %q, got %v", "a secret memo", sh.TextContent)
	}
	if sh.PasswordHash != nil {
		t.Errorf("PasswordHash: want nil for empty password, got %q", *sh.PasswordHash)
	}
}

// TestUploadSubmit_FileHappyPath: a well-formed kind=file submission lands
// a file share row with the storage object actually populated by the
// submitted bytes. The redirect target follows the same /shares/{id}/created
// shape as the text path, and OpenContent's reader streams the exact payload.
func TestUploadSubmit_FileHappyPath(t *testing.T) {
	srv, svc, handle := newUploadTestServerWithService(t, 1024*1024)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	payload := []byte("file payload bytes")
	req := newAuthedUploadRequest(t, sessionID, uploadFormSpec{
		Kind:     strPtr("file"),
		Password: strPtr(""),
		Expiry:   strPtr("3600"),
		File: &uploadFileSpec{
			Filename:    "report.pdf",
			ContentType: "application/pdf",
			Content:     payload,
		},
	})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/shares/") || !strings.HasSuffix(loc, "/created") {
		t.Fatalf("location: want /shares/{id}/created, got %q", loc)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(loc, "/shares/"), "/created")

	sh, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %q: %v", id, err)
	}
	if sh.Kind != "file" {
		t.Errorf("Kind: want %q, got %q", "file", sh.Kind)
	}
	if sh.OriginalFilename == nil || *sh.OriginalFilename != "report.pdf" {
		t.Errorf("OriginalFilename: want %q, got %v", "report.pdf", sh.OriginalFilename)
	}
	if sh.MIMEType == nil || *sh.MIMEType != "application/pdf" {
		t.Errorf("MIMEType: want %q, got %v", "application/pdf", sh.MIMEType)
	}
	if sh.SizeBytes == nil || *sh.SizeBytes != int64(len(payload)) {
		t.Errorf("SizeBytes: want %d, got %v", len(payload), sh.SizeBytes)
	}
	if sh.PasswordHash != nil {
		t.Errorf("PasswordHash: want nil for empty password, got %q", *sh.PasswordHash)
	}

	rc, err := svc.OpenContent(context.Background(), sh)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read storage object: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("storage bytes: want %q, got %q", payload, got)
	}
}

// TestUploadSubmit_WithPassword: setting a password on either kind must
// land a real bcrypt hash on the row — not the plaintext, not a no-op.
// CompareHashAndPassword is the authoritative check that the hash matches
// the original input, regardless of cost or algorithm changes downstream.
func TestUploadSubmit_WithPassword(t *testing.T) {
	cases := []struct {
		name string
		spec uploadFormSpec
	}{
		{
			name: "text",
			spec: uploadFormSpec{
				Kind:     strPtr("text"),
				Password: strPtr("hunter2"),
				Expiry:   strPtr("86400"),
				Text:     strPtr("private note"),
			},
		},
		{
			name: "file",
			spec: uploadFormSpec{
				Kind:     strPtr("file"),
				Password: strPtr("hunter2"),
				Expiry:   strPtr("86400"),
				File: &uploadFileSpec{
					Filename:    "secret.bin",
					ContentType: "application/octet-stream",
					Content:     []byte{1, 2, 3},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, svc, handle := newUploadTestServerWithService(t, 1024*1024)
			userID := insertUploadTestAdmin(t, handle)
			sessionID := uploadTestSession(t, handle, userID)

			req := newAuthedUploadRequest(t, sessionID, tc.spec)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status: want 303, got %d; body=%q", rec.Code, rec.Body.String())
			}
			loc := rec.Header().Get("Location")
			id := strings.TrimSuffix(strings.TrimPrefix(loc, "/shares/"), "/created")

			sh, err := svc.Get(context.Background(), id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if sh.PasswordHash == nil {
				t.Fatalf("PasswordHash: want non-nil for kind=%s with password set", tc.name)
			}
			if *sh.PasswordHash == "hunter2" {
				t.Errorf("PasswordHash: stored plaintext instead of bcrypt hash")
			}
			if err := bcrypt.CompareHashAndPassword([]byte(*sh.PasswordHash), []byte("hunter2")); err != nil {
				t.Errorf("CompareHashAndPassword: %v", err)
			}
		})
	}
}

// TestUploadSubmit_ValidationFailureRendersForm: a parse / validation
// failure (here: an expiry value not on the allowlist) must re-render
// upload.html with an Error banner at status 400 — not 500, not a redirect
// to a generic error page. The operator's intent should be preserved so
// they can fix the field and resubmit.
func TestUploadSubmit_ValidationFailureRendersForm(t *testing.T) {
	srv, _, handle := newUploadTestServerWithService(t, 1024*1024)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	req := newAuthedUploadRequest(t, sessionID, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("99999"),
		Text:   strPtr("hello"),
	})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`action="/upload"`,
		`enctype="multipart/form-data"`,
		`name="kind"`,
		`name="expiry"`,
		"could not process",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}

	var count int
	if err := handle.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares`).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 0 {
		t.Errorf("shares table: want 0 rows on validation failure, got %d", count)
	}
}

// TestUpload_StaticAssets: GET /static/upload.js must serve the embedded
// progress-bar / radio-toggle script with a JavaScript MIME type. The
// upload form references the file via <script src="/static/upload.js"> in
// the template, so a missing asset would silently break the JS-enhanced
// submit path without any unit-test coverage of the underlying handler.
// Substring-match on "javascript" covers both text/javascript and
// application/javascript in case a future Go release flips the default.
func TestUpload_StaticAssets(t *testing.T) {
	srv, _ := newUploadTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/static/upload.js", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content-type: want javascript MIME, got %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "XMLHttpRequest") {
		t.Errorf("body: want XHR-driven script, got:\n%s", body)
	}
}

// TestStatic_ServesCopyJS: GET /static/copy.js must serve the embedded
// clipboard helper with a JavaScript MIME type. share_created.html loads
// the file via <script src="/static/copy.js">, so a missing asset would
// silently disable the Copy button without any direct handler coverage.
// The substring check on "clipboard" pins the served body to the actual
// helper rather than just any javascript file.
func TestStatic_ServesCopyJS(t *testing.T) {
	srv, _ := newUploadTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/static/copy.js", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content-type: want javascript MIME, got %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "clipboard") {
		t.Errorf("body: want clipboard helper, got:\n%s", body)
	}
}

// TestStaticAssets_CopyJSReadsDataAttrs guards the Phase 11 i18n contract
// for the Copy button: the script must take its success / failure labels
// from data-copy-success-text and data-copy-failed-text on the button
// element, never from hardcoded English literals. share_created.html sets
// the attributes via i18n.T, so a regression here would silently re-pin
// the button feedback to English regardless of the user's language.
func TestStaticAssets_CopyJSReadsDataAttrs(t *testing.T) {
	srv, _ := newUploadTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/static/copy.js", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{"copySuccessText", "copyFailedText"} {
		if !strings.Contains(body, want) {
			t.Errorf("copy.js missing dataset read for %q; got:\n%s", want, body)
		}
	}
	for _, banned := range []string{"'Copied!'", `"Copied!"`, "'Copy failed'", `"Copy failed"`} {
		if strings.Contains(body, banned) {
			t.Errorf("copy.js still contains hardcoded English literal %q; got:\n%s", banned, body)
		}
	}
}

// TestUploadSubmit_TooLarge: a body exceeding MaxUploadBytes + headroom
// must surface as a friendly 413 with the upload form re-rendered, not a
// generic 500 or a hung connection. MaxBytesReader trips on read inside
// parseUploadForm; the handler errors.As-matches and routes to the
// dedicated "too large" page. Setting MaxUploadBytes to 1 here keeps the
// test payload manageable while still tripping the cap with room to spare.
func TestUploadSubmit_TooLarge(t *testing.T) {
	srv, _, handle := newUploadTestServerWithService(t, 1)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	bigText := strings.Repeat("a", 128*1024)
	req := newAuthedUploadRequest(t, sessionID, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr(bigText),
	})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: want 413, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "too large") {
		t.Errorf("body missing 'too large' message; got:\n%s", body)
	}
	if !strings.Contains(body, `action="/upload"`) {
		t.Errorf("body missing re-rendered upload form; got:\n%s", body)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares`).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 0 {
		t.Errorf("shares table: want 0 rows on too-large submission, got %d", count)
	}
}

// TestUploadSubmit_TooLarge_FileSpool covers the second MaxBytesError branch
// inside uploadSubmitHandler: parseUploadForm completes (non-file fields fit
// under the cap), but the file payload itself overflows during the spool
// io.Copy in createFileShareFromPart. The test pins the same friendly 413
// + form re-render mapping the parse-stage path produces, so a refactor that
// drops the second errors.As cannot land silently.
func TestUploadSubmit_TooLarge_FileSpool(t *testing.T) {
	srv, _, handle := newUploadTestServerWithService(t, 512)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	bigFile := bytes.Repeat([]byte("x"), 200*1024)
	req := newAuthedUploadRequest(t, sessionID, uploadFormSpec{
		Kind:   strPtr("file"),
		Expiry: strPtr("86400"),
		File: &uploadFileSpec{
			Filename:    "huge.bin",
			ContentType: "application/octet-stream",
			Content:     bigFile,
		},
	})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: want 413, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "too large") {
		t.Errorf("body missing 'too large' message; got:\n%s", body)
	}
	if !strings.Contains(body, `action="/upload"`) {
		t.Errorf("body missing re-rendered upload form; got:\n%s", body)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares`).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 0 {
		t.Errorf("shares table: want 0 rows on too-large file submission, got %d", count)
	}
}

// TestUploadSubmit_TextOverflow covers the size-aware banner for the
// per-field text cap. Pasted text between uploadFieldMaxBytes and
// MaxUploadBytes does not trip MaxBytesReader (the body fits the request
// cap) but does trip parseUploadForm's per-field cap. The handler must
// surface the text-specific message (mentioning the text limit) instead of
// the generic "could not process the form" line — pasting 100 KB and
// getting a vague 400 was the friction the friendly banner exists to fix.
func TestUploadSubmit_TextOverflow(t *testing.T) {
	srv, _, handle := newUploadTestServerWithService(t, 1024*1024)
	userID := insertUploadTestAdmin(t, handle)
	sessionID := uploadTestSession(t, handle, userID)

	bigText := strings.Repeat("a", uploadFieldMaxBytes+1)
	req := newAuthedUploadRequest(t, sessionID, uploadFormSpec{
		Kind:   strPtr("text"),
		Expiry: strPtr("86400"),
		Text:   strPtr(bigText),
	})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: want 413, got %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "text is too long") {
		t.Errorf("body missing text-specific 'too long' message; got:\n%s", body)
	}
	if !strings.Contains(body, `action="/upload"`) {
		t.Errorf("body missing re-rendered upload form; got:\n%s", body)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares`).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 0 {
		t.Errorf("shares table: want 0 rows on text-overflow submission, got %d", count)
	}
}
