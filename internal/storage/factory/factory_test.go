package factory

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/storage"
)

// TestNew_Local proves the factory's local branch returns something that
// satisfies the storage.Storage contract end-to-end. The R2 branch is
// deliberately not unit-tested here — it is covered by the integration test
// under internal/storage/r2 and by the r2.New empty-argument validation
// unit tests. What matters for the factory is that it wires the right
// constructor for each backend value; a full Put/Get roundtrip through the
// returned interface value is the smallest test that actually proves that
// for local.
func TestNew_Local(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Shared{
		StorageBackend:   config.StorageBackendLocal,
		StorageLocalPath: t.TempDir(),
	}

	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if store == nil {
		t.Fatalf("New returned nil Storage")
	}

	const (
		key         = "abc12345"
		contentType = "text/plain; charset=utf-8"
	)
	payload := []byte("hello factory")

	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), contentType); err != nil {
		t.Fatalf("Put: unexpected error: %v", err)
	}

	rc, info, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("info.Size = %d, want %d", info.Size, len(payload))
	}
	if info.ContentType != contentType {
		t.Fatalf("info.ContentType = %q, want %q", info.ContentType, contentType)
	}

	// compile-time-ish sanity check: the returned value must satisfy the
	// interface. The declared return type already guarantees this, but being
	// explicit guards against a future refactor that widens the return to
	// any / interface{}.
	var _ storage.Storage = store
}

// TestNew_NilConfig guards the defense-in-depth nil-check: hand-built
// callers (tests, future entry points) that pass a nil *config.Shared must
// get a real error, not a nil-dereference panic. Production callers always
// pass a non-nil struct from config.Load{Web,Bot}, but the cheap guard
// prevents a surprising crash if that ever changes.
func TestNew_NilConfig(t *testing.T) {
	ctx := context.Background()

	store, err := New(ctx, nil)
	if err == nil {
		t.Fatalf("New(nil): expected error, got nil (store=%v)", store)
	}
	if store != nil {
		t.Fatalf("New(nil): expected nil Storage on error, got %v", store)
	}
}

// TestNew_UnknownBackend makes sure the default arm of the switch is
// exercised: a hand-built Shared with a bogus backend value must produce an
// error mentioning the backend name, not a nil Storage or a panic. The
// config loader already rejects unknown values in production, but tests and
// future in-process construction paths can bypass that.
func TestNew_UnknownBackend(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Shared{StorageBackend: "bogus"}

	store, err := New(ctx, cfg)
	if err == nil {
		t.Fatalf("New: expected error for bogus backend, got nil (store=%v)", store)
	}
	if store != nil {
		t.Fatalf("New: expected nil Storage on error, got %v", store)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error %q does not mention the backend name 'bogus'", err)
	}
}

// captureLog returns a logger that writes JSON lines into buf, so tests can
// assert on the structured fields emitted by LogReady without binding to a
// particular text format.
func captureLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// TestLogReady_Local asserts the local branch emits the backend and path
// fields. Without this test, swapping the log field names (or dropping the
// branch entirely) would be silently fine as far as cmd/* tests care.
func TestLogReady_Local(t *testing.T) {
	var buf bytes.Buffer
	LogReady(captureLog(&buf), &config.Shared{
		StorageBackend:   config.StorageBackendLocal,
		StorageLocalPath: "/var/lib/yacht/files",
	})

	got := buf.String()
	for _, want := range []string{`"msg":"storage ready"`, `"backend":"local"`, `"path":"/var/lib/yacht/files"`} {
		if !strings.Contains(got, want) {
			t.Errorf("log output missing %q; got %s", want, got)
		}
	}
}

// TestLogReady_R2 asserts the R2 branch emits bucket and host-only endpoint.
// The host-only field is the whole point of endpointHost; a regression that
// logged the full URL (potentially including credentials in a future config
// change) would slip through without this assertion.
func TestLogReady_R2(t *testing.T) {
	var buf bytes.Buffer
	LogReady(captureLog(&buf), &config.Shared{
		StorageBackend: config.StorageBackendR2,
		R2Bucket:       "yacht-shares",
		R2Endpoint:     "https://acct.r2.cloudflarestorage.com/some/path",
	})

	got := buf.String()
	for _, want := range []string{`"backend":"r2"`, `"bucket":"yacht-shares"`, `"endpoint_host":"acct.r2.cloudflarestorage.com"`} {
		if !strings.Contains(got, want) {
			t.Errorf("log output missing %q; got %s", want, got)
		}
	}
	if strings.Contains(got, `"endpoint_host":"https://`) {
		t.Errorf("endpoint_host contains scheme, should be host-only; got %s", got)
	}
}

// TestLogReady_NilConfig guards the symmetric nil-check in LogReady: a
// hand-built caller that passes nil must not crash the startup process that
// would otherwise have reached a successful steady state. Because LogReady
// returns no error, we accept a warn line and a safe return as the contract.
func TestLogReady_NilConfig(t *testing.T) {
	var buf bytes.Buffer
	LogReady(captureLog(&buf), nil)

	got := buf.String()
	if !strings.Contains(got, `"level":"WARN"`) {
		t.Errorf("expected WARN level log; got %s", got)
	}
	if !strings.Contains(got, "nil config") {
		t.Errorf("expected nil-config mention; got %s", got)
	}
}

// TestEndpointHost covers the URL-parse helper directly: valid URL returns
// host, URL with credentials returns host (never the userinfo), and any input
// we cannot extract a host from collapses to the fixed "<unparseable>" marker
// so misconfigured endpoints (e.g. "user:pass@host" missing a scheme) can
// never slip credentials into the log line.
func TestEndpointHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with host", "https://acct.r2.cloudflarestorage.com", "acct.r2.cloudflarestorage.com"},
		{"https with path", "https://acct.r2.cloudflarestorage.com/bucket/key", "acct.r2.cloudflarestorage.com"},
		{"https with userinfo", "https://user:pass@acct.r2.cloudflarestorage.com", "acct.r2.cloudflarestorage.com"},
		{"empty string", "", "<unparseable>"},
		{"no scheme plain string", "not a url", "<unparseable>"},
		{"userinfo without scheme", "user:pass@host", "<unparseable>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointHost(tc.in); got != tc.want {
				t.Errorf("endpointHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
