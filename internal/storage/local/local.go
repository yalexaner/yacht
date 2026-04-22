// Package local is a filesystem-backed implementation of storage.Storage. It
// stores objects in a flat key layout under a root directory: the payload
// lives at <root>/<key> and a sidecar JSON document at <root>/<key>.meta.json
// records the authoritative size and content-type so Get can return them
// without re-sniffing on every read.
//
// The backend is intended for development and single-node deployments where
// dragging in an S3-compatible service is overkill. It deliberately does not
// support subdirectory partitioning — keys are share IDs (8-char nanoids,
// per SPEC), not user-controlled paths, so flat is simplest and fastest.
//
// Missing-object contract: Get and Delete return an error satisfying
// errors.Is(err, storage.ErrNotFound) when the primary payload file is
// absent. See package storage for the cross-backend contract.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/yalexaner/yacht/internal/storage"
)

// Backend is the filesystem implementation of storage.Storage. It is safe
// for concurrent use: individual operations are serialized by the kernel at
// the inode level, and Put's atomic-rename pattern guarantees readers never
// observe partial writes.
type Backend struct {
	root string
}

// compile-time interface assertion — if the Storage interface grows a new
// method, this line fails to build and forces us to update the backend
// (rather than silently diverging from R2).
var _ storage.Storage = (*Backend)(nil)

// metadata is the on-disk sidecar schema. Stored at <root>/<key>.meta.json,
// it carries the size and content-type so Get can return *storage.ObjectInfo
// without opening and sniffing the payload on every read. If the sidecar is
// missing or corrupt, Get falls back to os.Stat + http.DetectContentType (see
// Get).
type metadata struct {
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// sidecarSuffix is appended to the primary key filename to form the sidecar
// path. Kept as a constant so the naming convention has a single source of
// truth that validateKey and every I/O path agree on.
const sidecarSuffix = ".meta.json"

// New constructs a Backend rooted at root. It rejects an empty root (the
// Phase 1 config loader already checks this, but re-validating here keeps
// the backend honest if it's ever constructed outside of a config-driven
// path) and MkdirAll's the directory with 0o750 so the caller doesn't have
// to pre-create it. 0o750 is tighter than the 0o755 default because shared
// files may be private and the directory shouldn't leak its contents to
// world-readable enumeration on multi-user hosts.
func New(root string) (*Backend, error) {
	if root == "" {
		return nil, errors.New("local storage: root path is empty")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("local storage: mkdir %q: %w", root, err)
	}
	return &Backend{root: root}, nil
}

// validateKey enforces the flat-key invariant: keys map to a single file
// directly under root, with no subdirectories, no traversal, and no clash
// with the sidecar naming scheme. Share IDs are 8-char nanoids so none of
// these guards fire for legitimate callers — the point is defense in depth
// against a future caller (or a bug) that constructs a key from untrusted
// input.
func validateKey(key string) error {
	if key == "" {
		return errors.New("key is empty")
	}
	if strings.ContainsAny(key, `/\`) {
		return fmt.Errorf("key %q contains path separator", key)
	}
	if strings.Contains(key, "..") {
		return fmt.Errorf("key %q contains parent-directory segment", key)
	}
	if strings.HasPrefix(key, ".") {
		// leading dot would clash with sidecar naming (".meta.json") and with
		// hidden-file conventions on POSIX. Nanoids never start with a dot.
		return fmt.Errorf("key %q has leading dot", key)
	}
	if strings.HasSuffix(key, sidecarSuffix) {
		// a key like "foo.meta.json" would land at the same path as the sidecar
		// for key "foo", letting one Put silently overwrite another object's
		// metadata. Nanoids never end in ".meta.json" so this never fires for
		// legitimate callers, but validateKey is advertised as defense-in-depth
		// against untrusted input where this would be an exploitable collision.
		return fmt.Errorf("key %q has sidecar suffix", key)
	}
	return nil
}

// pathsFor returns (primary, sidecar) absolute paths for a given key. Only
// called after validateKey, so it is safe to filepath.Join without further
// scrubbing.
func (b *Backend) pathsFor(key string) (string, string) {
	primary := filepath.Join(b.root, key)
	sidecar := primary + sidecarSuffix
	return primary, sidecar
}

// Put streams r into <root>/<key> and writes the sidecar with the provided
// size and contentType. Writes go through a temp file in the same directory
// followed by os.Rename, which is atomic on POSIX when both paths live on
// the same filesystem — so readers either see the old state or the full new
// state, never a half-written file after a crash or concurrent Get.
func (b *Backend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := validateKey(key); err != nil {
		return fmt.Errorf("put: %w", err)
	}
	primary, sidecar := b.pathsFor(key)

	// capture the actual byte count from io.Copy so the sidecar records what
	// we really wrote, not what the caller claimed. If the two disagree the
	// caller lied (or the reader was truncated) and we prefer on-disk truth —
	// silently persisting a wrong caller-reported size would surface later as
	// a Content-Length / body-length mismatch on Get.
	var written int64
	if err := writeAtomic(primary, func(f *os.File) error {
		// io.Copy uses the reader's WriteTo / the writer's ReadFrom if
		// available, so large uploads stream without intermediate buffering.
		n, err := io.Copy(f, r)
		written = n
		return err
	}); err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	_ = size // size is accepted for interface symmetry with the R2 backend (S3 requires ContentLength up front); the local backend authoritatively uses written bytes instead.

	meta := metadata{Size: written, ContentType: contentType}
	if err := writeAtomic(sidecar, func(f *os.File) error {
		return json.NewEncoder(f).Encode(&meta)
	}); err != nil {
		// best-effort cleanup of the (potentially stale) sidecar so Get falls
		// back to os.Stat + http.DetectContentType instead of returning old
		// metadata for the newly-written payload. Do NOT remove the primary:
		// on an overwrite Put, the rename has already replaced the previous
		// payload, and removing it now would turn a transient sidecar-write
		// failure into permanent data loss of the new bytes. Readers tolerate
		// a missing sidecar via the fallback path in readObjectInfo.
		_ = os.Remove(sidecar)
		return fmt.Errorf("put %q sidecar: %w", key, err)
	}

	_ = ctx // context is part of the interface for backend symmetry; local I/O is not cancellable mid-syscall on POSIX.
	return nil
}

// writeAtomic creates a temp file next to path, lets write populate it, syncs
// to disk, and renames into place. The temp file lives in the same directory
// as the target so the rename stays on one filesystem (rename across
// filesystems falls back to copy+delete and is no longer atomic).
func writeAtomic(path string, write func(*os.File) error) (retErr error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	// belt-and-braces: if anything past this point fails, don't leak the
	// tmp file on disk. Close is harmless after a successful Close because
	// os.File.Close is idempotent-ish (returns ErrClosed, which we ignore).
	defer func() {
		if retErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := write(tmp); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	// fsync the data before rename so a crash between rename and the next
	// fsync doesn't leave an empty file in the target slot.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp into place: %w", err)
	}
	return nil
}

// Get opens the payload file and returns it as an io.ReadCloser alongside
// *ObjectInfo. On a missing primary file, the returned error satisfies
// errors.Is(err, storage.ErrNotFound). When the sidecar is missing or
// corrupt, Get falls back to os.Stat for Size and http.DetectContentType on
// the first 512 bytes of the payload — this keeps the backend robust against
// partial-state leftover from a crash mid-Put (payload renamed in, sidecar
// write failed) and against external tampering of sidecars.
func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, *storage.ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return nil, nil, fmt.Errorf("get: %w", err)
	}
	primary, sidecar := b.pathsFor(key)

	f, err := os.Open(primary)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("get %q: %w", key, storage.ErrNotFound)
		}
		return nil, nil, fmt.Errorf("get %q: open: %w", key, err)
	}

	info, err := readObjectInfo(f, sidecar)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("get %q: %w", key, err)
	}

	_ = ctx // see Put.
	return f, info, nil
}

// readObjectInfo resolves the ObjectInfo for an open payload file, preferring
// the sidecar JSON when present and well-formed, falling back to
// os.Stat + http.DetectContentType otherwise. The file's read offset is
// restored to 0 before returning so the caller's first Read sees the first
// byte of the payload regardless of which path we took.
func readObjectInfo(payload *os.File, sidecarPath string) (*storage.ObjectInfo, error) {
	if meta, err := readSidecar(sidecarPath); err == nil {
		return &storage.ObjectInfo{Size: meta.Size, ContentType: meta.ContentType}, nil
	}

	// sidecar missing or corrupt — reconstruct from the payload itself.
	stat, err := payload.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat payload: %w", err)
	}

	var sniff [512]byte
	n, err := io.ReadFull(payload, sniff[:])
	// io.ReadFull returns ErrUnexpectedEOF for short reads, which is expected
	// for payloads smaller than 512 bytes — treat it as success. Any other
	// error is a real read failure and gets surfaced.
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("sniff payload: %w", err)
	}
	if _, err := payload.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind payload: %w", err)
	}

	return &storage.ObjectInfo{
		Size:        stat.Size(),
		ContentType: http.DetectContentType(sniff[:n]),
	}, nil
}

// readSidecar opens and decodes the sidecar JSON, returning a non-nil error
// if anything about it is wrong (missing, empty, malformed JSON). The caller
// treats any error as "fall back to sniffing" — callers should not inspect
// the specific error.
func readSidecar(path string) (*metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var m metadata
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Delete removes the payload and (best-effort) its sidecar. If the primary
// is already gone, Delete returns an error satisfying
// errors.Is(err, storage.ErrNotFound) — symmetric with Get — so callers can
// distinguish "already deleted" from a real failure. The sidecar's absence
// is swallowed: the primary is the source of truth, and a torn state with a
// leftover sidecar is exactly what the Get fallback path is there to cope
// with anyway.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	primary, sidecar := b.pathsFor(key)

	if err := os.Remove(primary); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete %q: %w", key, storage.ErrNotFound)
		}
		return fmt.Errorf("delete %q: %w", key, err)
	}
	if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %q sidecar: %w", key, err)
	}

	_ = ctx // see Put.
	return nil
}
