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
