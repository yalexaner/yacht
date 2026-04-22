// Package share is the upload/download logic layer that ties internal/db and
// internal/storage together. Service is the single entry point: it generates
// share IDs, uploads payloads to storage, persists metadata in SQLite, hashes
// and verifies passwords, fetches shares, distinguishes expired from missing,
// opens content readers for both file and text shares, and increments
// download counts.
//
// Construction is deferred to the web and bot binaries' startup code (see
// Phase 6/7) — this package exposes only the logic surface. No handlers, no
// HTTP, no Telegram.
//
// Sentinel errors: Get, OpenContent, VerifyPassword, and IncrementDownloadCount
// return one of the exported sentinels below when the caller can act on the
// specific condition (share missing, expired, password mismatch, share has
// no password set). Every return site wraps the sentinel with fmt.Errorf
// "...: %w" so callers MUST use errors.Is — never equality, never a type
// assertion — to match. This lets the wrapping shape evolve without breaking
// consumers.
package share

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	gonanoid "github.com/matoous/go-nanoid/v2"
	"golang.org/x/crypto/bcrypt"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/storage"
)

// shareIDLength is the number of characters in a generated share ID. Eight
// characters of the nanoid default URL-safe alphabet (64 symbols) is ~47
// bits of entropy — at personal scale the birthday-collision probability
// remains effectively zero for the lifetime of the service, and the IDs are
// short enough to fit comfortably into URLs and Telegram captions.
const shareIDLength = 8

// Kind values written to the shares.kind column. Callers compare Share.Kind
// against these constants rather than string literals.
const (
	KindFile = "file"
	KindText = "text"
)

// Service is the share logic layer. Construct one per process via New; it is
// safe for concurrent use because every dependency it holds (*sql.DB,
// storage.Storage) is itself safe for concurrent use.
type Service struct {
	db      *sql.DB
	storage storage.Storage
	cfg     *config.Shared
}

// New builds a Service from already-initialized dependencies. It does not
// ping the database or the storage backend: db.New verified the SQLite
// connection at startup, and the storage factory exercised the backend's
// constructor — re-pinging here would only add startup latency without
// catching new failure modes.
func New(db *sql.DB, storage storage.Storage, cfg *config.Shared) *Service {
	return &Service{db: db, storage: storage, cfg: cfg}
}

// Share is the in-memory projection of a row in the shares table. Nullable
// DB columns are represented as pointers so the zero value is distinguishable
// from an explicitly-stored zero (e.g. a 0-byte file has SizeBytes = &0, a
// text share has SizeBytes = nil).
//
// Kind is always one of KindFile / KindText. For KindFile, StorageKey,
// OriginalFilename, MIMEType, and SizeBytes are non-nil and TextContent is
// nil. For KindText, TextContent is non-nil and the file-only fields are nil.
// PasswordHash is nil when the share was created without a password.
type Share struct {
	ID               string
	UserID           int64
	Kind             string
	OriginalFilename *string
	MIMEType         *string
	SizeBytes        *int64
	TextContent      *string
	StorageKey       *string
	PasswordHash     *string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	DownloadCount    int64
}

// CreateFileOpts is the input to Service.CreateFileShare.
//
// Password empty means "no password required". Expiry zero means "use
// cfg.DefaultExpiry"; a non-zero Expiry takes precedence over the config
// default so callers can implement per-share expiry overrides without
// touching the shared config.
type CreateFileOpts struct {
	UserID           int64
	OriginalFilename string
	MIMEType         string
	Size             int64
	Content          io.Reader
	Password         string
	Expiry           time.Duration
}

// CreateTextOpts is the input to Service.CreateTextShare. Password and
// Expiry follow the same sentinel semantics as CreateFileOpts.
type CreateTextOpts struct {
	UserID   int64
	Content  string
	Password string
	Expiry   time.Duration
}

// ErrNotFound is returned (wrapped) by Get and IncrementDownloadCount when
// the share ID has no row in the database. Match with errors.Is.
var ErrNotFound = errors.New("share: not found")

// ErrExpired is returned (wrapped) by Get when the row exists but its
// expires_at has already passed. Cleanup is async (see Phase 8), so expired
// rows linger in the table; the service surfaces them as ErrExpired rather
// than ErrNotFound so callers can distinguish "never existed" from "lapsed".
// Match with errors.Is.
var ErrExpired = errors.New("share: expired")

// ErrPasswordMismatch is returned (wrapped) by VerifyPassword when the
// supplied plaintext does not match the stored bcrypt hash. Match with
// errors.Is.
var ErrPasswordMismatch = errors.New("share: password mismatch")

// ErrNoPassword is returned by VerifyPassword when the share was created
// without a password and therefore has no hash to compare against. Callers
// shouldn't invoke VerifyPassword on such a share — receiving this error
// signals a caller-side control-flow bug. Match with errors.Is.
var ErrNoPassword = errors.New("share: no password set")

// newShareID returns a fresh 8-char share ID using the nanoid default URL-safe
// alphabet. Isolated so tests and future callers can swap in a deterministic
// generator without touching the CreateShare code paths.
func newShareID() (string, error) {
	id, err := gonanoid.New(shareIDLength)
	if err != nil {
		return "", fmt.Errorf("generate share id: %w", err)
	}
	return id, nil
}

// hashPassword returns a bcrypt hash of plaintext wrapped in a pointer, or
// (nil, nil) when plaintext is empty (the share has no password). bcrypt's
// DefaultCost=10 is the library's recommended default — fast enough on a
// small VPS to be unnoticeable per request, slow enough to make offline
// brute-force of leaked hashes costly.
func hashPassword(plaintext string) (*string, error) {
	if plaintext == "" {
		return nil, nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	s := string(h)
	return &s, nil
}

// CreateFileShare uploads opts.Content to the storage backend, persists a
// metadata row in the shares table, and returns a fully-hydrated *Share for
// the caller to build URLs / log / respond with. The returned ID is the
// same value used as the storage key (flat, no prefix — see SPEC).
//
// Upload-then-insert order: the object is written to storage BEFORE the
// database row is inserted. If the insert fails, the object is orphaned in
// storage; Phase 8's cleanup worker garbage-collects it by walking expired
// shares. The alternative (insert-then-upload) would produce DB rows
// without backing bytes, which surfaces on Get → OpenContent as
// storage.ErrNotFound — indistinguishable from actual data loss. Tolerating
// orphans is the better failure mode.
func (s *Service) CreateFileShare(ctx context.Context, opts CreateFileOpts) (*Share, error) {
	if opts.Content == nil {
		return nil, fmt.Errorf("create file share: content reader is nil")
	}
	if opts.Size < 0 {
		return nil, fmt.Errorf("create file share: negative size %d", opts.Size)
	}
	if opts.UserID == 0 {
		return nil, fmt.Errorf("create file share: user id is zero")
	}
	if opts.OriginalFilename == "" {
		return nil, fmt.Errorf("create file share: original filename is empty")
	}

	// zero Expiry means "use cfg default"; a non-zero Expiry lets callers
	// override per-share without mutating shared config.
	expiry := opts.Expiry
	if expiry == 0 {
		expiry = s.cfg.DefaultExpiry
	}

	id, err := newShareID()
	if err != nil {
		return nil, fmt.Errorf("create file share: %w", err)
	}

	passwordHash, err := hashPassword(opts.Password)
	if err != nil {
		return nil, fmt.Errorf("create file share: %w", err)
	}

	if err := s.storage.Put(ctx, id, opts.Content, opts.Size, opts.MIMEType); err != nil {
		return nil, fmt.Errorf("create file share: upload: %w", err)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(expiry)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO shares
			(id, user_id, kind, original_filename, mime_type, size_bytes,
			 storage_key, password_hash, created_at, expires_at, download_count)
		VALUES (?, ?, 'file', ?, ?, ?, ?, ?, ?, ?, 0)
	`, id, opts.UserID, opts.OriginalFilename, opts.MIMEType, opts.Size,
		id, passwordHash, now.Unix(), expiresAt.Unix())
	if err != nil {
		return nil, fmt.Errorf("create file share: insert: %w", err)
	}

	// re-hydrate locally rather than SELECT — every field was just written, so
	// a round-trip would only re-pay the integer->time conversion without
	// catching any bug that the INSERT alone wouldn't surface first.
	filename := opts.OriginalFilename
	mime := opts.MIMEType
	size := opts.Size
	storageKey := id
	return &Share{
		ID:               id,
		UserID:           opts.UserID,
		Kind:             KindFile,
		OriginalFilename: &filename,
		MIMEType:         &mime,
		SizeBytes:        &size,
		StorageKey:       &storageKey,
		PasswordHash:     passwordHash,
		CreatedAt:        time.Unix(now.Unix(), 0).UTC(),
		ExpiresAt:        time.Unix(expiresAt.Unix(), 0).UTC(),
		DownloadCount:    0,
	}, nil
}

// CreateTextShare persists a text snippet as a share. Unlike CreateFileShare
// there is no storage backend call — the payload lives in the shares.text_content
// column directly. storage_key, original_filename, mime_type, and size_bytes
// are all NULL for text shares; OpenContent serves the bytes from memory.
//
// This keeps small text snippets cheap to store and read: no object round-trip,
// no GC concerns, and the DB already provides atomicity between metadata and
// payload.
func (s *Service) CreateTextShare(ctx context.Context, opts CreateTextOpts) (*Share, error) {
	if opts.UserID == 0 {
		return nil, fmt.Errorf("create text share: user id is zero")
	}
	if opts.Content == "" {
		return nil, fmt.Errorf("create text share: content is empty")
	}

	// zero Expiry means "use cfg default"; a non-zero Expiry lets callers
	// override per-share without mutating shared config.
	expiry := opts.Expiry
	if expiry == 0 {
		expiry = s.cfg.DefaultExpiry
	}

	id, err := newShareID()
	if err != nil {
		return nil, fmt.Errorf("create text share: %w", err)
	}

	passwordHash, err := hashPassword(opts.Password)
	if err != nil {
		return nil, fmt.Errorf("create text share: %w", err)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(expiry)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO shares
			(id, user_id, kind, text_content, password_hash,
			 created_at, expires_at, download_count)
		VALUES (?, ?, 'text', ?, ?, ?, ?, 0)
	`, id, opts.UserID, opts.Content, passwordHash, now.Unix(), expiresAt.Unix())
	if err != nil {
		return nil, fmt.Errorf("create text share: insert: %w", err)
	}

	content := opts.Content
	return &Share{
		ID:            id,
		UserID:        opts.UserID,
		Kind:          KindText,
		TextContent:   &content,
		PasswordHash:  passwordHash,
		CreatedAt:     time.Unix(now.Unix(), 0).UTC(),
		ExpiresAt:     time.Unix(expiresAt.Unix(), 0).UTC(),
		DownloadCount: 0,
	}, nil
}
