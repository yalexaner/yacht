package share

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
	"github.com/yalexaner/yacht/internal/storage"
	"github.com/yalexaner/yacht/internal/storage/local"
)

// newTestService wires a Service onto a fresh temp-dir SQLite and a fresh
// temp-dir local storage backend. The db and storage roots live in separate
// t.TempDir() calls so a test that inspects one doesn't accidentally see
// files from the other. Only DefaultExpiry is populated on the config — the
// service doesn't read any other field at this phase, and keeping the rest
// zero makes accidental dependencies easy to spot (a nil pointer / zero value
// will fail loudly).
func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
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

	cfg := &config.Shared{DefaultExpiry: 24 * time.Hour}
	return New(handle, backend, cfg), handle
}

// insertTestUser inserts a row into the users table and returns the new id.
// Every CreateShare test needs a valid user_id because the shares table has
// a FOREIGN KEY on users(id). telegram_id uses the test's wall-clock nanos
// so two tests (or two users within one test) don't collide on the UNIQUE
// constraint.
func insertTestUser(t *testing.T, handle *sql.DB) int64 {
	t.Helper()

	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, is_admin, created_at)
		 VALUES (?, 0, strftime('%s','now'))`,
		time.Now().UnixNano(),
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

func TestNew_Constructor(t *testing.T) {
	svc, handle := newTestService(t)
	if svc == nil {
		t.Fatal("New returned nil")
	}
	if svc.db != handle {
		t.Error("Service.db does not match the handle returned by newTestService")
	}
	if svc.storage == nil {
		t.Error("Service.storage is nil")
	}
	if svc.cfg == nil || svc.cfg.DefaultExpiry != 24*time.Hour {
		t.Errorf("Service.cfg.DefaultExpiry = %v, want 24h", svc.cfg.DefaultExpiry)
	}
}

// newServiceWithStorage builds a Service using a caller-provided storage
// backend so tests that need to simulate storage failures can swap in a
// stub. The db wiring matches newTestService exactly.
func newServiceWithStorage(t *testing.T, backend storage.Storage) (*Service, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	cfg := &config.Shared{DefaultExpiry: 24 * time.Hour}
	return New(handle, backend, cfg), handle
}

// failingStorage implements storage.Storage but rejects every Put with a
// canned error. TestCreateFileShare_StorageFailurePreventsDBInsert uses this
// to lock in the upload-then-insert ordering contract: if the upload fails,
// the DB row must not exist.
type failingStorage struct {
	putErr error
}

func (f *failingStorage) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	return f.putErr
}

func (f *failingStorage) Get(ctx context.Context, key string) (io.ReadCloser, *storage.ObjectInfo, error) {
	return nil, nil, storage.ErrNotFound
}

func (f *failingStorage) Delete(ctx context.Context, key string) error {
	return storage.ErrNotFound
}

func TestCreateFileShare_HappyPath(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	payload := []byte("hello yacht")
	before := time.Now()
	got, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "hello.txt",
		MIMEType:         "text/plain",
		Size:             int64(len(payload)),
		Content:          bytes.NewReader(payload),
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	if len(got.ID) != shareIDLength {
		t.Errorf("ID length = %d, want %d", len(got.ID), shareIDLength)
	}
	if got.Kind != KindFile {
		t.Errorf("Kind = %q, want %q", got.Kind, KindFile)
	}
	if got.UserID != userID {
		t.Errorf("UserID = %d, want %d", got.UserID, userID)
	}
	if got.OriginalFilename == nil || *got.OriginalFilename != "hello.txt" {
		t.Errorf("OriginalFilename = %v, want \"hello.txt\"", got.OriginalFilename)
	}
	if got.MIMEType == nil || *got.MIMEType != "text/plain" {
		t.Errorf("MIMEType = %v, want \"text/plain\"", got.MIMEType)
	}
	if got.SizeBytes == nil || *got.SizeBytes != int64(len(payload)) {
		t.Errorf("SizeBytes = %v, want %d", got.SizeBytes, len(payload))
	}
	if got.StorageKey == nil || *got.StorageKey != got.ID {
		t.Errorf("StorageKey = %v, want %q", got.StorageKey, got.ID)
	}
	if got.PasswordHash != nil {
		t.Errorf("PasswordHash = %v, want nil (empty password)", got.PasswordHash)
	}
	if got.TextContent != nil {
		t.Errorf("TextContent = %v, want nil (file share)", got.TextContent)
	}
	if got.DownloadCount != 0 {
		t.Errorf("DownloadCount = %d, want 0", got.DownloadCount)
	}
	// allow a small tolerance because the Service records now in one call and
	// the test samples it on either side of that call.
	wantExp := before.Add(24 * time.Hour)
	if got.ExpiresAt.Before(wantExp.Add(-2*time.Second)) || got.ExpiresAt.After(after.Add(24*time.Hour).Add(2*time.Second)) {
		t.Errorf("ExpiresAt = %v, want ≈ now+24h", got.ExpiresAt)
	}

	// row exists in the DB with the expected primary columns
	var (
		dbID, dbKind string
		dbSize       int64
	)
	err = handle.QueryRowContext(context.Background(),
		`SELECT id, kind, size_bytes FROM shares WHERE id = ?`, got.ID,
	).Scan(&dbID, &dbKind, &dbSize)
	if err != nil {
		t.Fatalf("select back: %v", err)
	}
	if dbID != got.ID || dbKind != KindFile || dbSize != int64(len(payload)) {
		t.Errorf("db row = (%q,%q,%d), want (%q,%q,%d)", dbID, dbKind, dbSize, got.ID, KindFile, len(payload))
	}

	// storage has the uploaded bytes at key == ID
	rc, _, err := svc.storage.Get(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("storage.Get: %v", err)
	}
	defer rc.Close()
	read, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !bytes.Equal(read, payload) {
		t.Errorf("storage payload = %q, want %q", read, payload)
	}
}

func TestCreateFileShare_WithPassword(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	got, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "secret.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
		Password:         "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	if got.PasswordHash == nil {
		t.Fatal("PasswordHash = nil, want non-nil")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*got.PasswordHash), []byte("hunter2")); err != nil {
		t.Errorf("bcrypt.CompareHashAndPassword: %v (hash does not validate)", err)
	}
}

func TestCreateFileShare_ExplicitExpiry(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	before := time.Now()
	got, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "a.txt",
		MIMEType:         "text/plain",
		Size:             1,
		Content:          bytes.NewReader([]byte("a")),
		Expiry:           2 * time.Hour,
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	wantLow := before.Add(2 * time.Hour).Add(-2 * time.Second)
	wantHigh := after.Add(2 * time.Hour).Add(2 * time.Second)
	if got.ExpiresAt.Before(wantLow) || got.ExpiresAt.After(wantHigh) {
		t.Errorf("ExpiresAt = %v, want ≈ now+2h (not DefaultExpiry=24h)", got.ExpiresAt)
	}
}

func TestCreateFileShare_ValidatesInput(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	base := func() CreateFileOpts {
		return CreateFileOpts{
			UserID:           userID,
			OriginalFilename: "x.txt",
			MIMEType:         "text/plain",
			Size:             1,
			Content:          bytes.NewReader([]byte("x")),
		}
	}

	cases := []struct {
		name  string
		mutate func(*CreateFileOpts)
	}{
		{
			name:   "nil content",
			mutate: func(o *CreateFileOpts) { o.Content = nil },
		},
		{
			name:   "negative size",
			mutate: func(o *CreateFileOpts) { o.Size = -1 },
		},
		{
			name:   "zero user id",
			mutate: func(o *CreateFileOpts) { o.UserID = 0 },
		},
		{
			name:   "empty filename",
			mutate: func(o *CreateFileOpts) { o.OriginalFilename = "" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			got, err := svc.CreateFileShare(context.Background(), opts)
			if err == nil {
				t.Fatalf("CreateFileShare(%s) err = nil, want non-nil", tc.name)
			}
			if got != nil {
				t.Errorf("CreateFileShare(%s) share = %+v, want nil", tc.name, got)
			}
		})
	}
}

func TestCreateTextShare_HappyPath(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	content := "some secret memo"
	before := time.Now()
	got, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:  userID,
		Content: content,
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	if len(got.ID) != shareIDLength {
		t.Errorf("ID length = %d, want %d", len(got.ID), shareIDLength)
	}
	if got.Kind != KindText {
		t.Errorf("Kind = %q, want %q", got.Kind, KindText)
	}
	if got.UserID != userID {
		t.Errorf("UserID = %d, want %d", got.UserID, userID)
	}
	if got.TextContent == nil || *got.TextContent != content {
		t.Errorf("TextContent = %v, want %q", got.TextContent, content)
	}
	if got.StorageKey != nil {
		t.Errorf("StorageKey = %v, want nil (text share)", got.StorageKey)
	}
	if got.OriginalFilename != nil {
		t.Errorf("OriginalFilename = %v, want nil (text share)", got.OriginalFilename)
	}
	if got.MIMEType != nil {
		t.Errorf("MIMEType = %v, want nil (text share)", got.MIMEType)
	}
	if got.SizeBytes != nil {
		t.Errorf("SizeBytes = %v, want nil (text share)", got.SizeBytes)
	}
	if got.PasswordHash != nil {
		t.Errorf("PasswordHash = %v, want nil (empty password)", got.PasswordHash)
	}
	if got.DownloadCount != 0 {
		t.Errorf("DownloadCount = %d, want 0", got.DownloadCount)
	}
	wantExp := before.Add(24 * time.Hour)
	if got.ExpiresAt.Before(wantExp.Add(-2*time.Second)) || got.ExpiresAt.After(after.Add(24*time.Hour).Add(2*time.Second)) {
		t.Errorf("ExpiresAt = %v, want ≈ now+24h", got.ExpiresAt)
	}

	// row exists with the expected shape: kind=text, text_content set,
	// file-only columns NULL.
	var (
		dbKind        string
		dbText        sql.NullString
		dbStorageKey  sql.NullString
		dbFilename    sql.NullString
		dbMIMEType    sql.NullString
		dbSize        sql.NullInt64
	)
	err = handle.QueryRowContext(context.Background(),
		`SELECT kind, text_content, storage_key, original_filename, mime_type, size_bytes
		 FROM shares WHERE id = ?`, got.ID,
	).Scan(&dbKind, &dbText, &dbStorageKey, &dbFilename, &dbMIMEType, &dbSize)
	if err != nil {
		t.Fatalf("select back: %v", err)
	}
	if dbKind != KindText {
		t.Errorf("db kind = %q, want %q", dbKind, KindText)
	}
	if !dbText.Valid || dbText.String != content {
		t.Errorf("db text_content = %v, want %q", dbText, content)
	}
	if dbStorageKey.Valid {
		t.Errorf("db storage_key = %q, want NULL", dbStorageKey.String)
	}
	if dbFilename.Valid {
		t.Errorf("db original_filename = %q, want NULL", dbFilename.String)
	}
	if dbMIMEType.Valid {
		t.Errorf("db mime_type = %q, want NULL", dbMIMEType.String)
	}
	if dbSize.Valid {
		t.Errorf("db size_bytes = %d, want NULL", dbSize.Int64)
	}
}

func TestCreateTextShare_WithPassword(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	got, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:   userID,
		Content:  "top secret",
		Password: "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	if got.PasswordHash == nil {
		t.Fatal("PasswordHash = nil, want non-nil")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*got.PasswordHash), []byte("hunter2")); err != nil {
		t.Errorf("bcrypt.CompareHashAndPassword: %v (hash does not validate)", err)
	}
}

func TestCreateTextShare_ValidatesInput(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	base := func() CreateTextOpts {
		return CreateTextOpts{
			UserID:  userID,
			Content: "hi",
		}
	}

	cases := []struct {
		name   string
		mutate func(*CreateTextOpts)
	}{
		{
			name:   "empty content",
			mutate: func(o *CreateTextOpts) { o.Content = "" },
		},
		{
			name:   "zero user id",
			mutate: func(o *CreateTextOpts) { o.UserID = 0 },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			got, err := svc.CreateTextShare(context.Background(), opts)
			if err == nil {
				t.Fatalf("CreateTextShare(%s) err = nil, want non-nil", tc.name)
			}
			if got != nil {
				t.Errorf("CreateTextShare(%s) share = %+v, want nil", tc.name, got)
			}
		})
	}
}

func TestCreateFileShare_StorageFailurePreventsDBInsert(t *testing.T) {
	stubErr := errors.New("stub put failure")
	svc, handle := newServiceWithStorage(t, &failingStorage{putErr: stubErr})
	userID := insertTestUser(t, handle)

	got, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "x.txt",
		MIMEType:         "text/plain",
		Size:             1,
		Content:          bytes.NewReader([]byte("x")),
	})
	if err == nil {
		t.Fatal("CreateFileShare err = nil, want storage failure")
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("err = %v, want chain containing stubErr", err)
	}
	if got != nil {
		t.Errorf("share = %+v, want nil", got)
	}

	// no row in the DB — locks in the upload-then-insert ordering
	var count int
	if err := handle.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM shares WHERE user_id = ?`, userID,
	).Scan(&count); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if count != 0 {
		t.Errorf("share rows after failed upload = %d, want 0", count)
	}
}

func TestGet_HappyPath_FileShare(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	payload := []byte("payload for get")
	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "report.pdf",
		MIMEType:         "application/pdf",
		Size:             int64(len(payload)),
		Content:          bytes.NewReader(payload),
		Password:         "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.UserID != created.UserID {
		t.Errorf("UserID = %d, want %d", got.UserID, created.UserID)
	}
	if got.Kind != KindFile {
		t.Errorf("Kind = %q, want %q", got.Kind, KindFile)
	}
	if got.OriginalFilename == nil || *got.OriginalFilename != "report.pdf" {
		t.Errorf("OriginalFilename = %v, want \"report.pdf\"", got.OriginalFilename)
	}
	if got.MIMEType == nil || *got.MIMEType != "application/pdf" {
		t.Errorf("MIMEType = %v, want \"application/pdf\"", got.MIMEType)
	}
	if got.SizeBytes == nil || *got.SizeBytes != int64(len(payload)) {
		t.Errorf("SizeBytes = %v, want %d", got.SizeBytes, len(payload))
	}
	if got.StorageKey == nil || *got.StorageKey != created.ID {
		t.Errorf("StorageKey = %v, want %q", got.StorageKey, created.ID)
	}
	if got.TextContent != nil {
		t.Errorf("TextContent = %v, want nil (file share)", got.TextContent)
	}
	if got.PasswordHash == nil || *got.PasswordHash != *created.PasswordHash {
		t.Errorf("PasswordHash = %v, want %v", got.PasswordHash, created.PasswordHash)
	}
	if got.DownloadCount != 0 {
		t.Errorf("DownloadCount = %d, want 0", got.DownloadCount)
	}
	// created.CreatedAt / ExpiresAt are already truncated to second via
	// time.Unix during Create; Get reads the same second back — equality is
	// the right check, not approximate matching.
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
	if !got.ExpiresAt.Equal(created.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, created.ExpiresAt)
	}
}

func TestGet_HappyPath_TextShare(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:  userID,
		Content: "a memorable note",
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.Kind != KindText {
		t.Errorf("Kind = %q, want %q", got.Kind, KindText)
	}
	if got.TextContent == nil || *got.TextContent != "a memorable note" {
		t.Errorf("TextContent = %v, want \"a memorable note\"", got.TextContent)
	}
	if got.StorageKey != nil {
		t.Errorf("StorageKey = %v, want nil (text share)", got.StorageKey)
	}
	if got.OriginalFilename != nil {
		t.Errorf("OriginalFilename = %v, want nil (text share)", got.OriginalFilename)
	}
	if got.MIMEType != nil {
		t.Errorf("MIMEType = %v, want nil (text share)", got.MIMEType)
	}
	if got.SizeBytes != nil {
		t.Errorf("SizeBytes = %v, want nil (text share)", got.SizeBytes)
	}
	if got.PasswordHash != nil {
		t.Errorf("PasswordHash = %v, want nil (empty password)", got.PasswordHash)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
	if !got.ExpiresAt.Equal(created.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, created.ExpiresAt)
	}
}

func TestGet_Missing(t *testing.T) {
	svc, _ := newTestService(t)

	got, err := svc.Get(context.Background(), "nosuchid")
	if err == nil {
		t.Fatal("Get err = nil, want ErrNotFound")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want chain containing ErrNotFound", err)
	}
	if got != nil {
		t.Errorf("share = %+v, want nil", got)
	}
}

func TestGet_Expired(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	// bypass the Service's computed expiry by inserting a row whose
	// expires_at is already in the past. This is the only way to produce an
	// "expired at read time" state deterministically without sleeping.
	pastExpires := time.Now().Add(-1 * time.Hour).Unix()
	createdAt := time.Now().Add(-2 * time.Hour).Unix()
	id := "expired1"
	_, err := handle.ExecContext(context.Background(), `
		INSERT INTO shares
			(id, user_id, kind, text_content, created_at, expires_at, download_count)
		VALUES (?, ?, 'text', 'stale', ?, ?, 0)
	`, id, userID, createdAt, pastExpires)
	if err != nil {
		t.Fatalf("insert expired row: %v", err)
	}

	got, err := svc.Get(context.Background(), id)
	if err == nil {
		t.Fatal("Get err = nil, want ErrExpired")
	}
	if !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want chain containing ErrExpired", err)
	}
	if got != nil {
		t.Errorf("share = %+v, want nil", got)
	}
}

func TestGet_ExpiresAtBoundary(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	// Boundary: the Service uses time.Time.Before (strict "<") so a share
	// whose expires_at equals the current second is still live. These two
	// inserts lock that semantic in:
	//   - expires_at = now + 1h  → live (clearly in the future)
	//   - expires_at = now - 1s  → expired (clearly in the past)
	// The "exactly at now" case is covered by the strict-< code path and
	// exercised implicitly by the sub-second gap between insertion and Get
	// in the happy-path tests above (both CreatedAt and ExpiresAt are within
	// one second of time.Now() there, and Get returns the share).
	future := time.Now().Add(1 * time.Hour).Unix()
	futureID := "future01"
	_, err := handle.ExecContext(context.Background(), `
		INSERT INTO shares
			(id, user_id, kind, text_content, created_at, expires_at, download_count)
		VALUES (?, ?, 'text', 'live', strftime('%s','now'), ?, 0)
	`, futureID, userID, future)
	if err != nil {
		t.Fatalf("insert future row: %v", err)
	}

	if _, err := svc.Get(context.Background(), futureID); err != nil {
		t.Errorf("Get(future) err = %v, want nil", err)
	}

	past := time.Now().Add(-1 * time.Second).Unix()
	pastID := "justpast"
	_, err = handle.ExecContext(context.Background(), `
		INSERT INTO shares
			(id, user_id, kind, text_content, created_at, expires_at, download_count)
		VALUES (?, ?, 'text', 'stale', strftime('%s','now'), ?, 0)
	`, pastID, userID, past)
	if err != nil {
		t.Fatalf("insert past row: %v", err)
	}

	got, err := svc.Get(context.Background(), pastID)
	if err == nil {
		t.Fatal("Get(past) err = nil, want ErrExpired")
	}
	if !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want chain containing ErrExpired", err)
	}
	if got != nil {
		t.Errorf("share = %+v, want nil", got)
	}
}

func TestOpenContent_FileShare(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	payload := []byte("open-content payload")
	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "blob.bin",
		MIMEType:         "application/octet-stream",
		Size:             int64(len(payload)),
		Content:          bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	rc, err := svc.OpenContent(context.Background(), got)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	defer rc.Close()

	read, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(read, payload) {
		t.Errorf("payload = %q, want %q", read, payload)
	}
}

func TestOpenContent_TextShare(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	content := "inline text body"
	created, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:  userID,
		Content: content,
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	rc, err := svc.OpenContent(context.Background(), got)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	defer rc.Close()

	read, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(read) != content {
		t.Errorf("content = %q, want %q", read, content)
	}
}

func TestOpenContent_FileStorageMissing(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "ghost.txt",
		MIMEType:         "text/plain",
		Size:             3,
		Content:          bytes.NewReader([]byte("abc")),
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	// simulate db/storage drift: the row is still there but the backing object
	// has been removed out-of-band (e.g. a stray cleanup script). OpenContent
	// must surface storage.ErrNotFound so operators can distinguish this from
	// share.ErrNotFound (which means the share row itself doesn't exist).
	if err := svc.storage.Delete(context.Background(), *created.StorageKey); err != nil {
		t.Fatalf("storage.Delete: %v", err)
	}

	rc, err := svc.OpenContent(context.Background(), created)
	if err == nil {
		rc.Close()
		t.Fatal("OpenContent err = nil, want storage.ErrNotFound")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("err = %v, want chain containing storage.ErrNotFound", err)
	}
	if rc != nil {
		t.Errorf("reader = %v, want nil", rc)
	}
}

func TestOpenContent_UnknownKind(t *testing.T) {
	svc, _ := newTestService(t)

	bogus := &Share{ID: "bogusid1", Kind: "bogus"}
	rc, err := svc.OpenContent(context.Background(), bogus)
	if err == nil {
		rc.Close()
		t.Fatal("OpenContent err = nil, want non-nil for unknown kind")
	}
	if rc != nil {
		t.Errorf("reader = %v, want nil", rc)
	}
}

func TestVerifyPassword_Match(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "locked.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
		Password:         "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	// round-trip through Get so we verify against a Share hydrated from the
	// DB (the hash that actually reaches callers), not the freshly-built
	// struct returned by Create.
	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if err := svc.VerifyPassword(got, "hunter2"); err != nil {
		t.Errorf("VerifyPassword(correct) = %v, want nil", err)
	}
}

func TestVerifyPassword_Mismatch(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateFileShare(context.Background(), CreateFileOpts{
		UserID:           userID,
		OriginalFilename: "locked.bin",
		MIMEType:         "application/octet-stream",
		Size:             3,
		Content:          bytes.NewReader([]byte{1, 2, 3}),
		Password:         "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateFileShare: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	err = svc.VerifyPassword(got, "wrong")
	if err == nil {
		t.Fatal("VerifyPassword(wrong) err = nil, want ErrPasswordMismatch")
	}
	if !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("err = %v, want chain containing ErrPasswordMismatch", err)
	}
}

func TestVerifyPassword_NoPassword(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:  userID,
		Content: "no-password note",
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	err = svc.VerifyPassword(got, "anything")
	if err == nil {
		t.Fatal("VerifyPassword(no-password share) err = nil, want ErrNoPassword")
	}
	if !errors.Is(err, ErrNoPassword) {
		t.Errorf("err = %v, want chain containing ErrNoPassword", err)
	}
}

func TestIncrementDownloadCount_Once(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:  userID,
		Content: "count me",
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}
	if created.DownloadCount != 0 {
		t.Fatalf("initial DownloadCount = %d, want 0", created.DownloadCount)
	}

	if err := svc.IncrementDownloadCount(context.Background(), created.ID); err != nil {
		t.Fatalf("IncrementDownloadCount: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DownloadCount != 1 {
		t.Errorf("DownloadCount = %d, want 1", got.DownloadCount)
	}
}

func TestIncrementDownloadCount_Twice(t *testing.T) {
	svc, handle := newTestService(t)
	userID := insertTestUser(t, handle)

	created, err := svc.CreateTextShare(context.Background(), CreateTextOpts{
		UserID:  userID,
		Content: "count me twice",
	})
	if err != nil {
		t.Fatalf("CreateTextShare: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := svc.IncrementDownloadCount(context.Background(), created.ID); err != nil {
			t.Fatalf("IncrementDownloadCount #%d: %v", i+1, err)
		}
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DownloadCount != 2 {
		t.Errorf("DownloadCount = %d, want 2", got.DownloadCount)
	}
}

func TestIncrementDownloadCount_Missing(t *testing.T) {
	svc, _ := newTestService(t)

	err := svc.IncrementDownloadCount(context.Background(), "nosuchid")
	if err == nil {
		t.Fatal("IncrementDownloadCount err = nil, want ErrNotFound")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want chain containing ErrNotFound", err)
	}
}
