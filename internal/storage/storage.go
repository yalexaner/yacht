// Package storage is the object-storage abstraction that yacht's upload and
// download flows depend on. It hides the difference between the filesystem
// backend used in development (internal/storage/local) and the Cloudflare R2
// backend used in production (internal/storage/r2) behind a single Storage
// interface, and exposes a factory (internal/storage.New) that selects one
// based on cfg.StorageBackend at startup.
//
// Missing-object contract: both Get and Delete distinguish "the object does
// not exist" from every other failure mode by returning an error that
// satisfies errors.Is(err, ErrNotFound). Backends MAY wrap ErrNotFound with
// context (e.g. fmt.Errorf("get %q: %w", key, ErrNotFound)), but the
// wrapped chain MUST preserve errors.Is matching. Callers use errors.Is —
// never equality and never a type assertion — so backends can evolve their
// concrete error types without breaking consumers.
//
// Symmetry across backends is the whole point of the interface: the share
// service at the HTTP layer cannot know whether a key lives on local disk or
// in R2, and MUST NOT need to — that's why the S3 DeleteObject idempotency
// quirk is papered over by the R2 backend with a HeadObject pre-check, and
// why the local backend's sidecar ContentType resolution falls back to
// http.DetectContentType when the sidecar is missing. Both backends present
// the same contract: "if it was there, you get it; if it wasn't, you get
// ErrNotFound".
package storage

import (
	"context"
	"errors"
	"io"
)

// Storage is the contract every object-storage backend in this repo
// implements. Signatures match SPEC § Storage Interface verbatim and are
// deliberately minimal — no presigned URLs, no listing, no lifecycle; those
// are either backend-specific (R2 lifecycle rules are configured on the
// bucket, not through this interface) or not needed by the share service.
//
// Contract for Get/Delete on a missing key: return an error satisfying
// errors.Is(err, ErrNotFound). See package doc.
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

// ObjectInfo is the minimal metadata returned alongside the payload reader
// from Get. Size is the authoritative byte count for the payload stream,
// and ContentType is the MIME type the object was stored with (or a
// best-effort detection result on the local backend when the sidecar is
// missing — see internal/storage/local).
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// ErrNotFound is the sentinel every backend returns (possibly wrapped) when
// Get or Delete is called with a key that does not exist. Callers MUST use
// errors.Is(err, ErrNotFound) — not ==, not a type assertion — because
// backends are free to wrap this with context.
var ErrNotFound = errors.New("storage: object not found")
