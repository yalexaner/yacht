package share

// Cleanup is the background GC pass for expired data. Designed to be called
// on a ticker by the web binary (see SPEC § Background Workers). Per-cycle
// scope: expired shares, expired sessions, used/expired login tokens.
// Idempotent — safe to run mid-cycle, safe to run on a schema that's newly
// populated. Subsequent tasks in Phase 8 flesh out each of the three
// deletion paths; the initial shell simply establishes the method signature
// and the CleanupStats return shape so callers (cmd/web's ticker goroutine)
// can be wired up in parallel without waiting on the full implementation.

import (
	"context"
	"fmt"
)

// CleanupStats reports what a single Cleanup pass removed. All counters are
// cumulative for that pass only — each call starts from zero. Errors counts
// the number of rows that encountered a non-fatal per-row failure (e.g. a
// storage.Delete error other than ErrNotFound on a file share); such rows
// are left in place so the next cycle can retry. A non-nil error return
// from Cleanup is reserved for pass-fatal failures (e.g. the top-level
// SELECT query itself fails), not per-row issues.
type CleanupStats struct {
	SharesDeleted      int64
	SessionsDeleted    int64
	LoginTokensDeleted int64
	Errors             int64
}

// String returns a compact one-liner suitable for slog message bodies when
// the caller prefers a single formatted field over four structured attrs.
// cmd/web's ticker currently logs the structured attrs directly, so this
// helper is optional — it exists so tests and ad-hoc debugging get a
// human-readable form without re-deriving the format.
func (c CleanupStats) String() string {
	return fmt.Sprintf("shares=%d sessions=%d login_tokens=%d errors=%d",
		c.SharesDeleted, c.SessionsDeleted, c.LoginTokensDeleted, c.Errors)
}

// Cleanup runs one GC pass across the three expiry-bearing tables and
// returns a summary of what was removed. See the package-level comment at
// the top of this file for scope/idempotency guarantees. ctx is plumbed
// through to every DB and storage call so the pass exits promptly when the
// web binary receives SIGINT/SIGTERM mid-cycle.
//
// This is the Task 1 shell — it returns an empty CleanupStats and nil
// error. Tasks 2–4 fill in expired shares, expired sessions, and
// used/expired login tokens respectively.
func (s *Service) Cleanup(ctx context.Context) (CleanupStats, error) {
	_ = ctx
	return CleanupStats{}, nil
}
