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
	"database/sql"
	"errors"
	"io"
	"time"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/storage"
)

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
