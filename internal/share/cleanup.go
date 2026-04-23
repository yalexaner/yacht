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
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yalexaner/yacht/internal/storage"
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
// Task 4 will layer used/expired login-token deletes on top of the
// expired-shares and expired-sessions passes implemented here.
func (s *Service) Cleanup(ctx context.Context) (CleanupStats, error) {
	var stats CleanupStats

	if err := s.cleanupExpiredShares(ctx, &stats); err != nil {
		return stats, err
	}

	if err := s.cleanupExpiredSessions(ctx, &stats); err != nil {
		return stats, err
	}

	return stats, nil
}

// cleanupExpiredShares deletes every share whose expires_at is in the past.
// Order of operations per row is storage-first, then DB: we drop the backing
// object before the metadata row so we can never leave a DB row that points
// at a missing object (which would surface at download time as a confusing
// storage.ErrNotFound on an apparently-live share). If the storage.Delete
// fails with anything other than ErrNotFound we SKIP the DB delete — the
// row stays, cleanup retries next cycle, and the not-yet-deleted object is
// still reachable. ErrNotFound is treated as "already gone, proceed" so a
// prior partially-completed pass doesn't block further progress.
//
// Text shares have storage_key IS NULL — the payload lives inline in the
// shares.text_content column, so there's no storage side to clean up. The
// storage.Delete call is simply skipped for those rows.
//
// The SELECT runs without a LIMIT: at personal scale the expected per-cycle
// count is <100 rows, well inside a single statement's budget. Batching is
// a Phase 14 polish concern if ever warranted. A pass-fatal error (e.g. the
// SELECT itself failing, or a Scan error) is returned so the caller can log
// and the ticker can try again next tick. Per-row errors are counted into
// stats.Errors and do not abort the pass.
func (s *Service) cleanupExpiredShares(ctx context.Context, stats *CleanupStats) error {
	now := time.Now().Unix()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, storage_key FROM shares WHERE expires_at < ?`, now)
	if err != nil {
		return fmt.Errorf("cleanup: query expired shares: %w", err)
	}
	defer rows.Close()

	// buffer the full result set before mutating the table. Holding a live
	// *sql.Rows open across a DELETE on the same table produces undefined
	// behavior on some SQLite configurations (database is locked / iterator
	// invalidation); collecting first, then deleting, sidesteps the issue
	// without needing a transaction.
	type expiredShare struct {
		id         string
		kind       string
		storageKey sql.NullString
	}
	var expired []expiredShare
	for rows.Next() {
		var row expiredShare
		if err := rows.Scan(&row.id, &row.kind, &row.storageKey); err != nil {
			return fmt.Errorf("cleanup: scan expired share: %w", err)
		}
		expired = append(expired, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("cleanup: iterate expired shares: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("cleanup: close expired shares rows: %w", err)
	}

	for _, row := range expired {
		if row.storageKey.Valid {
			err := s.storage.Delete(ctx, row.storageKey.String)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				// skip DB delete so the next cycle can retry. The row still
				// points at its (possibly still-existing) object, which is
				// the safe state to linger in; the R2 60-day lifecycle rule
				// is the final backstop if this row is never reapable.
				stats.Errors++
				continue
			}
		}

		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM shares WHERE id = ?`, row.id); err != nil {
			// the storage object is already gone (or never existed for a
			// text share) — count the row as errored and move on. The next
			// cycle will re-issue the storage.Delete, which is a no-op that
			// returns ErrNotFound, and retry the DB delete.
			stats.Errors++
			continue
		}
		stats.SharesDeleted++
	}

	return nil
}

// cleanupExpiredSessions deletes every row in sessions whose expires_at is
// in the past. Sessions carry no external-resource side effects — the row
// IS the session — so a single DELETE statement is both necessary and
// sufficient. RowsAffected feeds stats.SessionsDeleted so callers see how
// much reclaimed space this pass produced even though there's no per-row
// work to narrate. A non-nil error is returned so the caller can log and
// the ticker retries next tick; there's no per-row error path because
// there's no per-row work.
func (s *Service) cleanupExpiredSessions(ctx context.Context, stats *CleanupStats) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("cleanup: delete expired sessions: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cleanup: expired sessions rows affected: %w", err)
	}
	stats.SessionsDeleted = n
	return nil
}
