package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yalexaner/yacht/internal/storage"
)

// newForTest constructs a Backend rooted in t.TempDir(). Centralizing this
// means each test body focuses on its assertion, not boilerplate.
func newForTest(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	return b
}

// TestNew_RejectsEmptyRoot locks in the constructor's empty-root guard — the
// config loader is the primary defense here, but a second layer keeps the
// backend honest if it's ever constructed directly.
func TestNew_RejectsEmptyRoot(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatalf("New(\"\") returned nil error, want error")
	}
}

// TestNew_MkdirAll confirms New creates the root directory when it does not
// exist yet, so callers don't have to pre-provision it.
func TestNew_MkdirAll(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	if _, err := New(root); err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("root %q is not a directory", root)
	}
}

// TestBackend_PutGetRoundTrip proves the happy path: bytes in, bytes out,
// Size and ContentType preserved through the sidecar.
func TestBackend_PutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	payload := []byte("hello yacht")
	const (
		key = "abc12345"
		ct  = "text/plain; charset=utf-8"
	)

	if err := b.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), ct); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("info.Size = %d, want %d", info.Size, len(payload))
	}
	if info.ContentType != ct {
		t.Errorf("info.ContentType = %q, want %q", info.ContentType, ct)
	}
}

// TestBackend_GetMissing confirms the not-found contract for Get: an
// unknown key returns an error that satisfies errors.Is(err, ErrNotFound),
// not a raw os.ErrNotExist that would leak the backend implementation.
func TestBackend_GetMissing(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	rc, info, err := b.Get(ctx, "nopenope")
	if err == nil {
		_ = rc.Close()
		t.Fatalf("Get missing: nil error, want ErrNotFound")
	}
	if rc != nil || info != nil {
		t.Errorf("Get missing returned non-nil reader or info: rc=%v info=%v", rc, info)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get missing: errors.Is(err, ErrNotFound) = false; err = %v", err)
	}
}

// TestBackend_DeleteMissing mirrors the Get contract on Delete: callers can
// use errors.Is(err, ErrNotFound) to tell "already gone" from a real failure.
func TestBackend_DeleteMissing(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	err := b.Delete(ctx, "nopenope")
	if err == nil {
		t.Fatalf("Delete missing: nil error, want ErrNotFound")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Delete missing: errors.Is(err, ErrNotFound) = false; err = %v", err)
	}
}

// TestBackend_PutDeleteGet exercises the full lifecycle: a key we just
// deleted must read back as ErrNotFound — confirms Delete actually removes
// the primary and that Get honours the contract on the resulting state.
func TestBackend_PutDeleteGet(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	const key = "lifecycl"
	if err := b.Put(ctx, key, strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rc, _, err := b.Get(ctx, key)
	if err == nil {
		_ = rc.Close()
		t.Fatalf("Get after Delete: nil error, want ErrNotFound")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get after Delete: errors.Is(err, ErrNotFound) = false; err = %v", err)
	}
}

// TestBackend_RejectsPathTraversal table-drives every documented key-shape
// that the backend is supposed to refuse. Each call must fail on Put, Get,
// and Delete — the validation lives at the top of every op, so dropping it
// from any one of them should fail one of these sub-tests.
func TestBackend_RejectsPathTraversal(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"forward slash", "sub/dir"},
		{"back slash", `sub\dir`},
		{"parent dir", "../escape"},
		{"leading dot", ".hidden"},
		{"sidecar collision leading", ".meta.json"},
		{"sidecar collision suffix", "abc12345.meta.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := b.Put(ctx, tc.key, strings.NewReader("x"), 1, "text/plain"); err == nil {
				t.Errorf("Put(%q): nil error, want validation failure", tc.key)
			}
			if _, _, err := b.Get(ctx, tc.key); err == nil {
				t.Errorf("Get(%q): nil error, want validation failure", tc.key)
			}
			if err := b.Delete(ctx, tc.key); err == nil {
				t.Errorf("Delete(%q): nil error, want validation failure", tc.key)
			}
		})
	}
}

// TestBackend_SidecarCorruptFallback overwrites the sidecar with junk and
// confirms Get still succeeds by falling back to os.Stat + DetectContentType.
// This locks in the robustness contract documented on readObjectInfo — if
// someone "simplifies" Get by deleting the fallback, partial-state leftovers
// from a crashed Put turn into user-facing 500s again.
func TestBackend_SidecarCorruptFallback(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	// PNG signature so DetectContentType has something stable to recognize.
	pngSig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	const key = "pic00001"

	if err := b.Put(ctx, key, bytes.NewReader(pngSig), int64(len(pngSig)), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// corrupt the sidecar — truncate to non-JSON garbage so the decoder fails.
	sidecar := filepath.Join(b.root, key+sidecarSuffix)
	if err := os.WriteFile(sidecar, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt sidecar: %v", err)
	}

	rc, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after corrupt sidecar: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !bytes.Equal(got, pngSig) {
		t.Errorf("payload changed after corruption fallback")
	}
	if info.Size != int64(len(pngSig)) {
		t.Errorf("fallback Size = %d, want %d", info.Size, len(pngSig))
	}
	// DetectContentType returns "image/png" for a PNG signature — the exact
	// string is stable across Go versions, so asserting it here is fine.
	if info.ContentType != "image/png" {
		t.Errorf("fallback ContentType = %q, want %q", info.ContentType, "image/png")
	}
}

// TestBackend_SidecarMissingFallback removes the sidecar entirely (simulating
// a crash mid-Put that landed the primary but not the metadata) and confirms
// Get's os.Stat + DetectContentType fallback handles it just like the corrupt
// case. Mirrors the partial-state scenario documented on readObjectInfo.
func TestBackend_SidecarMissingFallback(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	payload := []byte("plain ascii body")
	const key = "nosidcar"

	if err := b.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Remove(filepath.Join(b.root, key+sidecarSuffix)); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	rc, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	if info.Size != int64(len(payload)) {
		t.Errorf("fallback Size = %d, want %d", info.Size, len(payload))
	}
	if info.ContentType == "" {
		t.Errorf("fallback ContentType is empty; DetectContentType should always return something")
	}
}

// TestBackend_PutOverwrite proves that a second Put on the same key cleanly
// replaces both the payload and the sidecar metadata. Without this, a
// regression where the second rename leaked stale sidecar fields (e.g.
// keeping the old ContentType) would silently corrupt Get responses.
func TestBackend_PutOverwrite(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	const key = "overwrit"
	first := []byte("first payload")
	second := []byte("second payload, different length")

	if err := b.Put(ctx, key, bytes.NewReader(first), int64(len(first)), "text/plain"); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := b.Put(ctx, key, bytes.NewReader(second), int64(len(second)), "image/png"); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	rc, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Errorf("payload = %q, want %q", got, second)
	}
	if info.Size != int64(len(second)) {
		t.Errorf("info.Size = %d, want %d", info.Size, len(second))
	}
	if info.ContentType != "image/png" {
		t.Errorf("info.ContentType = %q, want %q", info.ContentType, "image/png")
	}
}

// TestBackend_OverwriteSidecarFailureKeepsPrimary proves that when a Put
// overwrites a prior object and the sidecar write fails, the new payload
// stays on disk and remains readable via Get's fallback path. Regression
// guard for a previous shape that removed the primary on sidecar failure,
// which turned a transient sidecar I/O failure on an overwrite into
// permanent data loss of the new bytes.
func TestBackend_OverwriteSidecarFailureKeepsPrimary(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	const key = "ovrfail1"
	first := []byte("first payload")
	second := []byte("second payload, different length")
	sidecar := filepath.Join(b.root, key+sidecarSuffix)

	if err := b.Put(ctx, key, bytes.NewReader(first), int64(len(first)), "text/plain"); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	// replace the sidecar with a directory so the second Put's sidecar
	// rename cannot land. The primary rename still succeeds because nothing
	// blocks it, so v2 bytes end up on disk under the key.
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	if err := os.Mkdir(sidecar, 0o750); err != nil {
		t.Fatalf("mkdir at sidecar path: %v", err)
	}

	if err := b.Put(ctx, key, bytes.NewReader(second), int64(len(second)), "image/png"); err == nil {
		t.Fatalf("second Put: expected sidecar rename to fail over a directory, got nil")
	}

	// the Put's cleanup removes the (empty) sidecar directory; belt-and-braces
	// in case a future change stops doing that.
	_ = os.RemoveAll(sidecar)

	rc, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after failed overwrite: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Errorf("payload = %q, want second %q (primary was destroyed by overeager cleanup)", got, second)
	}
	if info.Size != int64(len(second)) {
		t.Errorf("info.Size = %d, want %d (fallback should report actual file size)", info.Size, len(second))
	}
}

// TestBackend_DeleteLeavesSidecarCleanup sanity-checks that a successful
// Delete removes both the primary and the sidecar from disk. Without this,
// a stale sidecar could accumulate on disk over time and confuse a future
// external auditor.
func TestBackend_DeleteLeavesSidecarCleanup(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	const key = "cleanup1"
	if err := b.Put(ctx, key, strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(filepath.Join(b.root, key)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("primary still present after Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.root, key+sidecarSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sidecar still present after Delete: %v", err)
	}
}

// TestBackend_PutIgnoresWrongCallerSize asserts the sidecar records the
// actually-copied byte count, not the caller-reported size. Trusting the
// caller's size blindly would let a miscount or a truncated reader persist a
// wrong Size into metadata and surface later as a Content-Length / body-length
// mismatch on Get.
func TestBackend_PutIgnoresWrongCallerSize(t *testing.T) {
	ctx := context.Background()
	b := newForTest(t)

	payload := []byte("exactly eleven")
	const key = "sizechk1"

	// caller lies about size (claims way more than the reader delivers).
	if err := b.Put(ctx, key, bytes.NewReader(payload), 9999, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	if info.Size != int64(len(payload)) {
		t.Errorf("info.Size = %d, want %d (actual bytes written)", info.Size, len(payload))
	}
}

// TestValidateKey is a direct unit test of the validation predicate so a
// change to the allowed shape shows up as a single focused failure rather
// than cascading through every op-level test.
func TestValidateKey(t *testing.T) {
	ok := []string{"abc12345", "A1B2C3D4", "x", "8charids"}
	for _, k := range ok {
		if err := validateKey(k); err != nil {
			t.Errorf("validateKey(%q) = %v, want nil", k, err)
		}
	}
	bad := []string{"", "a/b", `a\b`, "..", "a..b", ".hidden", "/abs", "abc12345.meta.json"}
	for _, k := range bad {
		if err := validateKey(k); err == nil {
			t.Errorf("validateKey(%q) = nil, want error", k)
		}
	}
}
