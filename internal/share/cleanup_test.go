package share

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/storage"
	"github.com/yalexaner/yacht/internal/storage/local"
)

// TestCleanup_EmptyTables locks in the baseline contract: running a GC pass
// against a freshly migrated database with no rows returns a zero-value
// CleanupStats and no error. Each subsequent task in Phase 8 layers a new
// deletion path on top; this test ensures none of them spuriously bump a
// counter or surface a false-positive error when there's nothing to do.
func TestCleanup_EmptyTables(t *testing.T) {
	svc, _ := newTestService(t)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SharesDeleted != 0 {
		t.Errorf("SharesDeleted = %d, want 0", stats.SharesDeleted)
	}
	if stats.SessionsDeleted != 0 {
		t.Errorf("SessionsDeleted = %d, want 0", stats.SessionsDeleted)
	}
	if stats.LoginTokensDeleted != 0 {
		t.Errorf("LoginTokensDeleted = %d, want 0", stats.LoginTokensDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
}

// TestCleanupStats_String locks the compact one-liner format so future edits
// don't silently change what ad-hoc debugging output or log messages look
// like. The cmd/web ticker uses structured slog attrs rather than this
// helper, but the helper is part of the package surface.
func TestCleanupStats_String(t *testing.T) {
	got := CleanupStats{
		SharesDeleted:      2,
		SessionsDeleted:    3,
		LoginTokensDeleted: 4,
		Errors:             1,
	}.String()
	want := "shares=2 sessions=3 login_tokens=4 errors=1"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// expireShare rewrites the expires_at column on an existing row to a time
// in the past. Tests use this rather than sleeping for a real expiry or
// shrinking DefaultExpiry so the "share is expired" state is produced
// deterministically and without tying cleanup semantics to wall-clock
// precision.
func expireShare(t *testing.T, handle *sql.DB, id string) {
	t.Helper()

	past := time.Now().Add(-1 * time.Hour).Unix()
	if _, err := handle.ExecContext(context.Background(),
		`UPDATE shares SET expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("expireShare(%q): %v", id, err)
	}
}

// insertSessionRow inserts a row directly into the sessions table with the
// given expires_at offset (seconds relative to now; negative = expired).
// Sessions aren't produced by any code path until Phase 9 adds the auth
// layer, so cleanup tests build the fixture by raw insert rather than via
// a public constructor. Returns the generated session id so tests can
// assert row presence/absence afterwards.
func insertSessionRow(t *testing.T, handle *sql.DB, userID int64, expiresAtOffset time.Duration) string {
	t.Helper()

	now := time.Now()
	id := fmt.Sprintf("sess-%d", now.UnixNano())
	if _, err := handle.ExecContext(context.Background(),
		`INSERT INTO sessions (id, user_id, provider, expires_at, created_at)
		 VALUES (?, ?, 'telegram_widget', ?, ?)`,
		id, userID, now.Add(expiresAtOffset).Unix(), now.Unix()); err != nil {
		t.Fatalf("insertSessionRow: %v", err)
	}
	return id
}

// insertLoginToken inserts a row directly into the login_tokens table with
// the given used_at and expires_at offsets (both relative to now; a nil
// usedAtOffset leaves used_at NULL, a negative expires offset = expired).
// Login tokens aren't produced by any code path until Phase 9 adds the
// auth layer, so cleanup tests build the fixture by raw insert. Returns
// the generated token so tests can assert row presence/absence.
func insertLoginToken(t *testing.T, handle *sql.DB, userID int64, usedAtOffset *time.Duration, expiresAtOffset time.Duration) string {
	t.Helper()

	now := time.Now()
	token := fmt.Sprintf("tok-%d", now.UnixNano())

	var usedAt sql.NullInt64
	if usedAtOffset != nil {
		usedAt = sql.NullInt64{Int64: now.Add(*usedAtOffset).Unix(), Valid: true}
	}

	if _, err := handle.ExecContext(context.Background(),
		`INSERT INTO login_tokens (token, user_id, used_at, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		token, userID, usedAt, now.Add(expiresAtOffset).Unix(), now.Unix()); err != nil {
		t.Fatalf("insertLoginToken: %v", err)
	}
	return token
}

// TestCleanup_ExpiredFileShare covers the storage-then-DB delete path for
// a file share whose expires_at has passed. After Cleanup, both the
// storage backend and the shares row must be gone, and stats must reflect
// exactly one deletion with no per-row errors.
func TestCleanup_ExpiredFileShare(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "doomed.txt",
		MIMEType:         "text/plain",
		Size:             4,
		Content:          bytes.NewReader([]byte("bye!")),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}
	expireShare(t, handle, created.ID)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SharesDeleted != 1 {
		t.Errorf("SharesDeleted = %d, want 1", stats.SharesDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	rc, _, getErr := svc.storage.Get(context.Background(), *created.StorageKey)
	if getErr == nil {
		rc.Close()
		t.Errorf("storage.Get(%q) succeeded after cleanup; want ErrNotFound", *created.StorageKey)
	} else if !errors.Is(getErr, storage.ErrNotFound) {
		t.Errorf("storage.Get err = %v, want ErrNotFound", getErr)
	}

	_, svcErr := svc.Get(context.Background(), created.ID)
	if !errors.Is(svcErr, ErrNotFound) {
		t.Errorf("service.Get err = %v, want ErrNotFound", svcErr)
	}
}

// TestCleanup_ExpiredTextShare confirms text shares are reaped via a
// straight DB delete — no storage.Delete call is made (they have no
// storage key to target). We prove this with a recordingDeleteStorage
// wrapper that appends every Delete key; the assertion is that the
// wrapper records nothing across the cleanup pass.
func TestCleanup_ExpiredTextShare(t *testing.T) {
	inner, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	rec := &recordingDeleteStorage{inner: inner}
	svc, handle := newServiceWithStorage(t, rec)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:  userID,
		Content: "fleeting thought",
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}
	expireShare(t, handle, created.ID)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SharesDeleted != 1 {
		t.Errorf("SharesDeleted = %d, want 1", stats.SharesDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
	if len(rec.deleted) != 0 {
		t.Errorf("storage.Delete called with %v, want no calls (text share has no storage key)", rec.deleted)
	}

	_, svcErr := svc.Get(context.Background(), created.ID)
	if !errors.Is(svcErr, ErrNotFound) {
		t.Errorf("service.Get err = %v, want ErrNotFound", svcErr)
	}
}

// TestCleanup_ActiveShareUntouched confirms the expires_at < now filter is
// exclusive: a share whose expiry sits in the future survives the pass
// with storage and DB intact and stats untouched.
func TestCleanup_ActiveShareUntouched(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "keep.txt",
		MIMEType:         "text/plain",
		Size:             4,
		Content:          bytes.NewReader([]byte("keep")),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SharesDeleted != 0 {
		t.Errorf("SharesDeleted = %d, want 0", stats.SharesDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("service.Get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("Get returned ID %q, want %q", got.ID, created.ID)
	}
}

// TestCleanup_StorageErrorSkipsDBDelete locks in the "retry next cycle"
// failure mode: when storage.Delete returns any error other than
// ErrNotFound, Cleanup must NOT remove the DB row — otherwise we'd be
// left with an orphaned object nobody tracks. stats.Errors bumps, the
// shares row stays put, and the object remains reachable on the storage
// backend.
func TestCleanup_StorageErrorSkipsDBDelete(t *testing.T) {
	inner, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	stubErr := errors.New("simulated storage delete failure")
	backend := &deleteFailingStorage{inner: inner, deleteErr: stubErr}
	svc, handle := newServiceWithStorage(t, backend)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "stuck.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}
	expireShare(t, handle, created.ID)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.Errors != 1 {
		t.Errorf("Errors = %d, want 1", stats.Errors)
	}
	if stats.SharesDeleted != 0 {
		t.Errorf("SharesDeleted = %d, want 0 (DB delete must be skipped on storage error)", stats.SharesDeleted)
	}

	// DB row still present so the next cycle can retry.
	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM shares WHERE id = ?`, created.ID).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 1 {
		t.Errorf("shares row count for %q = %d, want 1", created.ID, count)
	}

	// storage object still present — the simulated Delete failed, so the
	// inner backend never ran Delete and the bytes are still readable.
	rc, _, getErr := inner.Get(context.Background(), *created.StorageKey)
	if getErr != nil {
		t.Errorf("inner.Get after failed cleanup: %v (want object still present)", getErr)
	} else {
		rc.Close()
	}
}

// TestCleanup_StorageErrNotFoundProceedsWithDBDelete covers the "already
// gone" case: if storage.Delete reports ErrNotFound (e.g. a prior cycle
// partially completed, or an operator ran a manual bucket cleanup),
// Cleanup must treat that as success and proceed to the DB delete. The
// row ends up gone and no per-row error is counted.
func TestCleanup_StorageErrNotFoundProceedsWithDBDelete(t *testing.T) {
	inner, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	backend := &deleteFailingStorage{inner: inner, deleteErr: storage.ErrNotFound}
	svc, handle := newServiceWithStorage(t, backend)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "ghost.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}
	expireShare(t, handle, created.ID)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SharesDeleted != 1 {
		t.Errorf("SharesDeleted = %d, want 1", stats.SharesDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0 (ErrNotFound is not an error from cleanup's POV)", stats.Errors)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM shares WHERE id = ?`, created.ID).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 0 {
		t.Errorf("shares row count for %q = %d, want 0 (DB delete must run)", created.ID, count)
	}
}

// TestCleanup_ExpiredSession covers the happy path for session GC: a row
// whose expires_at has already passed is removed and the deletion shows
// up in stats.SessionsDeleted. Uses insertSessionRow because the Phase 9
// auth layer that produces sessions doesn't exist yet.
func TestCleanup_ExpiredSession(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)
	id := insertSessionRow(t, handle, userID, -1*time.Hour)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SessionsDeleted != 1 {
		t.Errorf("SessionsDeleted = %d, want 1", stats.SessionsDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("sessions row count for %q = %d, want 0", id, count)
	}
}

// TestCleanup_ActiveSessionUntouched confirms the expires_at < now filter
// is exclusive: a session whose expiry sits in the future survives the
// pass with the row intact and stats.SessionsDeleted unchanged.
func TestCleanup_ActiveSessionUntouched(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)
	id := insertSessionRow(t, handle, userID, 1*time.Hour)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SessionsDeleted != 0 {
		t.Errorf("SessionsDeleted = %d, want 0", stats.SessionsDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("sessions row count for %q = %d, want 1", id, count)
	}
}

// TestCleanup_UsedLoginToken covers the "single-use redeemed" branch of
// the login_tokens cleanup: a token whose used_at is non-null must be
// removed even if its expires_at still sits in the future, because a
// redeemed token can never be redeemed again.
func TestCleanup_UsedLoginToken(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)
	usedAt := -1 * time.Minute // used a moment ago
	token := insertLoginToken(t, handle, userID, &usedAt, 1*time.Hour)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.LoginTokensDeleted != 1 {
		t.Errorf("LoginTokensDeleted = %d, want 1", stats.LoginTokensDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM login_tokens WHERE token = ?`, token).Scan(&count); err != nil {
		t.Fatalf("count login_tokens: %v", err)
	}
	if count != 0 {
		t.Errorf("login_tokens row count for %q = %d, want 0", token, count)
	}
}

// TestCleanup_ExpiredLoginToken covers the "unused but past its window"
// branch: a token with used_at NULL and expires_at in the past must be
// reaped so the table doesn't accumulate dead rows. This is the case that
// mirrors the sessions cleanup lifecycle.
func TestCleanup_ExpiredLoginToken(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)
	token := insertLoginToken(t, handle, userID, nil, -1*time.Hour)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.LoginTokensDeleted != 1 {
		t.Errorf("LoginTokensDeleted = %d, want 1", stats.LoginTokensDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM login_tokens WHERE token = ?`, token).Scan(&count); err != nil {
		t.Fatalf("count login_tokens: %v", err)
	}
	if count != 0 {
		t.Errorf("login_tokens row count for %q = %d, want 0", token, count)
	}
}

// TestCleanup_ActiveLoginTokenUntouched confirms the WHERE clause is
// precise: a token that's both unused (used_at NULL) and unexpired
// (expires_at in the future) survives the pass and stats stay at zero.
func TestCleanup_ActiveLoginTokenUntouched(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)
	token := insertLoginToken(t, handle, userID, nil, 1*time.Hour)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.LoginTokensDeleted != 0 {
		t.Errorf("LoginTokensDeleted = %d, want 0", stats.LoginTokensDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM login_tokens WHERE token = ?`, token).Scan(&count); err != nil {
		t.Fatalf("count login_tokens: %v", err)
	}
	if count != 1 {
		t.Errorf("login_tokens row count for %q = %d, want 1", token, count)
	}
}

// TestCleanup_CancelledContextReturnsPromptly locks in the shutdown
// contract relied on by cmd/web's ticker goroutine: when the signal-aware
// ctx is cancelled, an in-flight Cleanup pass unblocks quickly rather than
// running to completion. Every DB/storage call in Cleanup is ctx-aware, so
// the very first QueryContext against an already-cancelled ctx surfaces
// context.Canceled wrapped through fmt.Errorf. The test pre-cancels, calls
// Cleanup, and asserts the error chain plus a wall-clock bound so a future
// refactor that accidentally blocks on a non-ctx primitive fails loudly.
func TestCleanup_CancelledContextReturnsPromptly(t *testing.T) {
	svc, _ := newTestService(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// one second is roughly 1000x the expected return time on any machine
	// that can run this test suite at all — plenty of headroom without
	// letting a regression hide behind "eh, tests are slow".
	const bound = 1 * time.Second
	start := time.Now()
	_, err := svc.Cleanup(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Cleanup(cancelled ctx) err = nil, want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, err = %v", err)
	}
	if elapsed > bound {
		t.Errorf("Cleanup took %s on cancelled ctx, want < %s", elapsed, bound)
	}
}

// recordingDeleteStorage wraps a real backend so tests can assert whether
// Delete was invoked (and with which keys). Put and Get are straight
// passthroughs; only Delete is instrumented.
type recordingDeleteStorage struct {
	inner   storage.Storage
	deleted []string
}

func (r *recordingDeleteStorage) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	return r.inner.Put(ctx, key, body, size, contentType)
}

func (r *recordingDeleteStorage) Get(ctx context.Context, key string) (io.ReadCloser, *storage.ObjectInfo, error) {
	return r.inner.Get(ctx, key)
}

func (r *recordingDeleteStorage) Delete(ctx context.Context, key string) error {
	r.deleted = append(r.deleted, key)
	return r.inner.Delete(ctx, key)
}

// deleteFailingStorage passes Put/Get through to an inner backend (so
// CreateFileShare can succeed normally and the object lands on disk) but
// intercepts Delete and returns a caller-supplied error. Tests use this
// to simulate both transient storage failures (stubErr) and the
// already-gone case (storage.ErrNotFound) without needing a mock that
// lies about Put succeeding.
type deleteFailingStorage struct {
	inner     storage.Storage
	deleteErr error
}

func (f *deleteFailingStorage) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	return f.inner.Put(ctx, key, body, size, contentType)
}

func (f *deleteFailingStorage) Get(ctx context.Context, key string) (io.ReadCloser, *storage.ObjectInfo, error) {
	return f.inner.Get(ctx, key)
}

func (f *deleteFailingStorage) Delete(ctx context.Context, key string) error {
	return f.deleteErr
}
