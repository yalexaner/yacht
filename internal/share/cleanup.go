package share

// Cleanup is the background GC pass for expired data. Designed to be called
// on a ticker by the web binary (see SPEC § Background Workers). Per-cycle
// scope: expired shares, expired sessions, used/expired login tokens.
// Idempotent — safe to interrupt mid-cycle, safe to run on an empty schema.

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

// Cleanup runs one GC pass across the three expiry-bearing tables and
// returns a summary of what was removed. See the package-level comment at
// the top of this file for scope/idempotency guarantees. ctx is plumbed
// through to every DB and storage call so the pass exits promptly when the
// web binary receives SIGINT/SIGTERM mid-cycle.
func (s *Service) Cleanup(ctx context.Context) (CleanupStats, error) {
	var stats CleanupStats

	if err := s.cleanupExpiredShares(ctx, &stats); err != nil {
		return stats, err
	}

	if err := s.cleanupExpiredSessions(ctx, &stats); err != nil {
		return stats, err
	}

	if err := s.cleanupLoginTokens(ctx, &stats); err != nil {
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
		`SELECT id, storage_key FROM shares WHERE expires_at < ?`, now)
	if err != nil {
		return fmt.Errorf("cleanup: query expired shares: %w", err)
	}
	defer rows.Close()

	// buffer the full result set before mutating the table. database/sql
	// normally issues the DELETE on a separate pooled connection so an open
	// SELECT cursor wouldn't block it, but collecting upfront keeps the
	// loop independent of connection-pool internals (e.g. a future switch
	// to a single-conn Tx for batching) and makes per-row error handling
	// easier to reason about.
	type expiredShare struct {
		id         string
		storageKey sql.NullString
	}
	var expired []expiredShare
	for rows.Next() {
		var row expiredShare
		if err := rows.Scan(&row.id, &row.storageKey); err != nil {
			return fmt.Errorf("cleanup: scan expired share: %w", err)
		}
		expired = append(expired, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("cleanup: iterate expired shares: %w", err)
	}

	for _, row := range expired {
		// abort the pass on ctx cancel rather than iterating the remaining
		// rows and counting every ctx-cancelled storage/DB op as a row
		// error. Without this check, a shutdown mid-cycle inflates
		// stats.Errors by the row count and delays the goroutine exit by
		// the time it takes to plow through the buffered slice.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cleanup: expired shares: %w", err)
		}

		if row.storageKey.Valid {
			err := s.storage.Delete(ctx, row.storageKey.String)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				// a ctx-cancel landing on this row mid-call is shutdown,
				// not a row-level failure: short-circuit so stats.Errors
				// isn't inflated for what is really the pass-fatal path.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fmt.Errorf("cleanup: expired shares: %w", ctxErr)
				}
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
			// same shutdown short-circuit as the storage.Delete branch: a
			// ctx-cancelled DB op is the pass ending, not a row error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("cleanup: expired shares: %w", ctxErr)
			}
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

// cleanupLoginTokens deletes every row in login_tokens that has reached
// end-of-life — either used_at IS NOT NULL (single-use token that's been
// redeemed, per SPEC § Auth → Login token) or expires_at < now (unused but
// past its window). Both conditions are terminal: a used token can never be
// reused and an expired-unused token can never be used, so neither belongs
// in the live table. Collapsing them into one DELETE keeps the pass O(1)
// queries even though they're conceptually distinct lifecycles. Like
// sessions, there's no external side effect to clean up; a single statement
// plus RowsAffected fully expresses the operation.
func (s *Service) cleanupLoginTokens(ctx context.Context, stats *CleanupStats) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM login_tokens WHERE used_at IS NOT NULL OR expires_at < ?`,
		time.Now().Unix())
	if err != nil {
		return fmt.Errorf("cleanup: delete login tokens: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cleanup: login tokens rows affected: %w", err)
	}
	stats.LoginTokensDeleted = n
	return nil
}
