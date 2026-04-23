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
// Sentinel errors: Get, VerifyPassword, and IncrementDownloadCount return
// one of the exported sentinels below when the caller can act on the specific
// condition (share missing, expired, password mismatch, share has no password
// set). OpenContent does NOT translate storage-layer failures into share
// sentinels; it forwards storage.ErrNotFound unchanged so operators can
// distinguish db/storage drift from "share never existed". Every sentinel
// return site wraps with fmt.Errorf "...: %w" so callers MUST use errors.Is —
// never equality, never a type assertion — to match. This lets the wrapping
// shape evolve without breaking consumers.
package share

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
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

// allocateShareID returns a fresh ID that does not yet exist in the shares
// table. With ~47 bits of entropy from an 8-char nanoid the loop body almost
// never runs more than once at personal scale, but a pre-upload existence
// check is cheap insurance against the cross-share corruption that would
// otherwise follow from the upload-then-insert design: a colliding ID would
// see s.storage.Put overwrite the existing share's bytes before the INSERT
// failed on PRIMARY KEY, leaving the original share permanently pointing at
// the wrong payload. The check races with concurrent CreateShare calls
// (TOCTOU between this SELECT and the eventual INSERT), but at personal scale
// a sub-microsecond collision window between two concurrent creations is not
// a realistic failure mode. The retry cap exists only as defense against an
// otherwise infinite loop if the table somehow filled the keyspace.
func (s *Service) allocateShareID(ctx context.Context) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		id, err := newShareID()
		if err != nil {
			return "", err
		}
		var seen int
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM shares WHERE id = ?`, id).Scan(&seen)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", fmt.Errorf("check share id collision: %w", err)
		}
	}
	return "", errors.New("allocate share id: 5 collisions in a row")
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
// database row is inserted. The alternative (insert-then-upload) would
// produce DB rows without backing bytes, which surfaces on Get →
// OpenContent as storage.ErrNotFound — indistinguishable from actual data
// loss to the user. Tolerating storage orphans on insert failure is the
// better failure mode, and we minimize the orphan window two ways:
//
//   - allocateShareID does a pre-upload SELECT so a colliding ID can't make
//     storage.Put silently overwrite an existing share's bytes (which would
//     be cross-share corruption — far worse than an orphan).
//   - on insert failure we do a best-effort storage.Delete to drop the just-
//     uploaded object. This is safe because the pre-check guarantees we're
//     the only writer of this key in the no-concurrent-collision case.
//
// Anything still leaking past those two lines (process crash between Put
// and INSERT, Delete failure on a real disk error) falls through to the R2
// 60-day lifecycle rule documented in SPEC § Background Workers as the
// final safety net.
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
	if opts.Expiry < 0 {
		return nil, fmt.Errorf("create file share: negative expiry %v", opts.Expiry)
	}

	// zero Expiry means "use cfg default"; a non-zero Expiry lets callers
	// override per-share without mutating shared config.
	expiry := opts.Expiry
	if expiry == 0 {
		expiry = s.cfg.DefaultExpiry
	}

	id, err := s.allocateShareID(ctx)
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
		// best-effort cleanup of the just-uploaded object so a transient DB
		// failure doesn't leak storage. Safe because allocateShareID's pre-
		// check guarantees we own this key (in the no-concurrent-collision
		// case, which is effectively all cases at personal scale). Swallow
		// any Delete error: the orphan that remains is what the R2 60-day
		// lifecycle is configured to catch.
		//
		// Use a detached context so cleanup still runs when the caller's ctx
		// has already been cancelled (client disconnect, shutdown). Bounded
		// timeout keeps a slow Delete from stalling the handler return —
		// cleanup is best-effort, not load-bearing.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = s.storage.Delete(cleanupCtx, id)
		cancel()
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
	if opts.Expiry < 0 {
		return nil, fmt.Errorf("create text share: negative expiry %v", opts.Expiry)
	}

	// zero Expiry means "use cfg default"; a non-zero Expiry lets callers
	// override per-share without mutating shared config.
	expiry := opts.Expiry
	if expiry == 0 {
		expiry = s.cfg.DefaultExpiry
	}

	id, err := s.allocateShareID(ctx)
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

// Get fetches a share by ID. It distinguishes three states:
//   - the row doesn't exist → ErrNotFound (wrapped)
//   - the row exists but its expires_at has already passed → ErrExpired
//     (wrapped, returned Share is nil — callers must not use an expired
//     share's content)
//   - the row exists and is live → (*Share, nil)
//
// The expiry comparison is strict "<" via time.Time.Before at nanosecond
// precision. expires_at is stored at second resolution (nanos=0 on read),
// while time.Now() carries sub-second nanos, so a share becomes expired the
// first nanosecond of the Unix second stored in expires_at — not at the end
// of it. Example: expires_at=1_700_000_000 flips to ErrExpired as soon as
// time.Now() reaches 1_700_000_000.000000001. This is the intended semantic:
// callers see the share stay live through every second strictly before the
// boundary, then transition cleanly at the boundary itself.
//
// Cleanup of expired rows is async (see Phase 8), so expired rows linger in
// the table — surfacing them as ErrExpired rather than ErrNotFound lets
// callers show the user "this share has expired" instead of the more
// alarming "this share never existed".
func (s *Service) Get(ctx context.Context, id string) (*Share, error) {
	var (
		share                          Share
		originalFilename, mimeType     sql.NullString
		textContent, storageKey, hash  sql.NullString
		sizeBytes                      sql.NullInt64
		createdAt, expiresAt           int64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, kind, original_filename, mime_type, size_bytes,
		       text_content, storage_key, password_hash,
		       created_at, expires_at, download_count
		FROM shares
		WHERE id = ?
	`, id).Scan(
		&share.ID, &share.UserID, &share.Kind,
		&originalFilename, &mimeType, &sizeBytes,
		&textContent, &storageKey, &hash,
		&createdAt, &expiresAt, &share.DownloadCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", id, err)
	}

	share.OriginalFilename = nullStringToPtr(originalFilename)
	share.MIMEType = nullStringToPtr(mimeType)
	share.SizeBytes = nullInt64ToPtr(sizeBytes)
	share.TextContent = nullStringToPtr(textContent)
	share.StorageKey = nullStringToPtr(storageKey)
	share.PasswordHash = nullStringToPtr(hash)
	share.CreatedAt = time.Unix(createdAt, 0).UTC()
	share.ExpiresAt = time.Unix(expiresAt, 0).UTC()

	if share.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("get %q: %w", id, ErrExpired)
	}
	return &share, nil
}

// OpenContent returns a reader for the share's payload. For a file share the
// reader streams from the storage backend; for a text share it wraps the
// in-memory text_content column with io.NopCloser. Callers are responsible
// for closing the returned reader.
//
// Storage-layer ErrNotFound on a file share is forwarded unchanged rather
// than being translated into share.ErrNotFound. A missing object when the
// database row exists means storage and the DB are out of sync — a different
// failure mode from "this share was never created" (ErrNotFound on Get) or
// "this share lapsed" (ErrExpired on Get). Blurring the two would rob
// operators of the signal they need to spot drift between the two systems.
//
// Unknown Kind values are a should-never-happen: Kind is only written by
// CreateFileShare and CreateTextShare, and both use the package constants.
// The branch exists as defense-in-depth so a corrupt row produces a clear
// error rather than a nil-dereference panic on the pointer fields.
func (s *Service) OpenContent(ctx context.Context, sh *Share) (io.ReadCloser, error) {
	switch sh.Kind {
	case KindFile:
		if sh.StorageKey == nil {
			return nil, fmt.Errorf("open content %q: file share has nil storage key", sh.ID)
		}
		rc, _, err := s.storage.Get(ctx, *sh.StorageKey)
		if err != nil {
			return nil, fmt.Errorf("open content %q: %w", sh.ID, err)
		}
		return rc, nil
	case KindText:
		if sh.TextContent == nil {
			return nil, fmt.Errorf("open content %q: text share has nil text content", sh.ID)
		}
		return io.NopCloser(strings.NewReader(*sh.TextContent)), nil
	default:
		return nil, fmt.Errorf("open content %q: unknown kind %q", sh.ID, sh.Kind)
	}
}

// VerifyPassword checks plaintext against the share's stored bcrypt hash.
// Return values:
//   - nil — the password matches
//   - ErrNoPassword (wrapped) — the share has no password set; calling
//     VerifyPassword on it is a caller-side control-flow bug (the caller
//     should check PasswordHash == nil first and skip the prompt entirely)
//   - ErrPasswordMismatch (wrapped) — bcrypt rejected the plaintext
//   - any other error (wrapped) — a corrupt or malformed hash; operationally
//     this means the row is unusable and needs manual investigation
//
// This method returns error rather than bool so the three distinct "not a
// match" states stay distinguishable at the call site via errors.Is.
func (s *Service) VerifyPassword(sh *Share, plaintext string) error {
	if sh.PasswordHash == nil {
		return fmt.Errorf("verify password %q: %w", sh.ID, ErrNoPassword)
	}
	err := bcrypt.CompareHashAndPassword([]byte(*sh.PasswordHash), []byte(plaintext))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return fmt.Errorf("verify password %q: %w", sh.ID, ErrPasswordMismatch)
	}
	if err != nil {
		return fmt.Errorf("verify password %q: %w", sh.ID, err)
	}
	return nil
}

// IncrementDownloadCount atomically bumps the share's download_count by one.
// Returns ErrNotFound (wrapped) when no row matches the ID — callers that
// care about the distinction between "share missing" and "share present but
// not yet incremented" can match with errors.Is.
//
// The UPDATE runs unconditionally on the row: expired shares still get their
// counter bumped. Callers gate on Get (which returns ErrExpired) before
// invoking this method, so reaching here implies the share was live when the
// download started; a cleanup worker racing with us is a benign drift that
// doesn't warrant special-casing here.
func (s *Service) IncrementDownloadCount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shares SET download_count = download_count + 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("increment %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("increment %q: rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("increment %q: %w", id, ErrNotFound)
	}
	return nil
}

// nullStringToPtr converts a sql.NullString into *string: nil when the
// column was NULL, a fresh pointer to the string otherwise. Keeps the Scan
// site free of boilerplate.
func nullStringToPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

// nullInt64ToPtr converts a sql.NullInt64 into *int64: nil when the column
// was NULL, a fresh pointer to the value otherwise.
func nullInt64ToPtr(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64
	return &v
}
