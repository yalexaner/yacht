//go:build integration

package r2

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/storage"
)

// r2FromEnv builds a Backend from R2_* env vars, skipping the test when any
// of them is unset. Keeping the skip here (instead of at the test level)
// means every integration test gets the same hermetic behaviour without
// repeating the env-var enumeration.
func r2FromEnv(t *testing.T) *Backend {
	t.Helper()

	required := map[string]string{
		"R2_ACCOUNT_ID":        os.Getenv("R2_ACCOUNT_ID"),
		"R2_ACCESS_KEY_ID":     os.Getenv("R2_ACCESS_KEY_ID"),
		"R2_SECRET_ACCESS_KEY": os.Getenv("R2_SECRET_ACCESS_KEY"),
		"R2_BUCKET":            os.Getenv("R2_BUCKET"),
		"R2_ENDPOINT":          os.Getenv("R2_ENDPOINT"),
	}
	for name, v := range required {
		if v == "" {
			t.Skipf("R2 integration test skipped: %s is not set", name)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	b, err := New(ctx,
		required["R2_ACCOUNT_ID"],
		required["R2_ACCESS_KEY_ID"],
		required["R2_SECRET_ACCESS_KEY"],
		required["R2_BUCKET"],
		required["R2_ENDPOINT"],
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// randomKey returns a hex-encoded random key prefixed with "yacht-test-" so
// concurrent CI runs against the same scratch bucket cannot collide. The
// prefix also makes orphaned objects (from test failures before cleanup
// runs) easy to spot in the bucket UI.
func randomKey(t *testing.T) string {
	t.Helper()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "yacht-test-" + hex.EncodeToString(buf[:])
}

// TestBackend_R2Roundtrip exercises the full Put → Get → Delete → Get(miss)
// path against a real bucket. The assertions mirror the local backend's
// TestBackend_PutGetRoundTrip so any contract drift between the two shows up
// here — which is the whole point of running integration tests.
func TestBackend_R2Roundtrip(t *testing.T) {
	b := r2FromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	key := randomKey(t)
	payload := []byte("r2 integration payload\n")
	const contentType = "text/plain; charset=utf-8"

	// best-effort cleanup: if the test fails partway through, don't leave
	// objects behind in the bucket. A second Delete on a missing key
	// returns ErrNotFound, which is fine — we only care that the common
	// case cleans up.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := b.Delete(cleanupCtx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			t.Logf("cleanup delete %q: %v", key, err)
		}
	})

	if err := b.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), contentType); err != nil {
		t.Fatalf("Put: %v", err)
	}

	body, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("bytes mismatch: got %q, want %q", got, payload)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("info.Size = %d, want %d", info.Size, len(payload))
	}
	if info.ContentType != contentType {
		t.Errorf("info.ContentType = %q, want %q", info.ContentType, contentType)
	}

	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, err = b.Get(ctx, key)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}
