package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// XRefreshCoverage contains metadata only. PendingRemoval covers the archive's
// cleanup fence, because readers cannot expose a stale connected copy.
type XRefreshCoverage struct {
	Outstanding       int64
	OldestUncheckedAt time.Time
	PendingRemoval    bool
}

const xLatestRefreshAlias = `NOT EXISTS (SELECT 1 FROM x_post_versions n
 WHERE n.source_id=v.source_id AND n.canonical_id=v.canonical_id
 AND (n.version_position>v.version_position OR
 (n.version_position=v.version_position AND (LENGTH(n.post_id)>LENGTH(v.post_id)
 OR (LENGTH(n.post_id)=LENGTH(v.post_id) AND n.post_id>v.post_id)))))`

const xRefreshNeedsCheck = `(v.suppressed_at IS NULL AND
 (v.last_checked_at IS NULL OR v.last_checked_at<? OR v.availability='unverified'))`

// XRefreshCandidatesContext returns at most one latest-known native ID per
// chain. A successful write removes it from this frozen sweep; an unresolved
// row stays eligible independently of any diagnostic cursor.
func (s *Store) XRefreshCandidatesContext(ctx context.Context, sourceID int64, before time.Time, limit int) (ids []string, coverage XRefreshCoverage, err error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return nil, coverage, err
	}
	if sourceID <= 0 || before.IsZero() || limit < 1 || limit > 100 {
		return nil, coverage, errors.New("invalid X refresh selection")
	}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		where := `v.source_id=? AND ` + xLatestRefreshAlias + ` AND (` + xRefreshNeedsCheck + ` OR v.cleanup_pending=1)`
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM x_post_versions v WHERE `+where, sourceID, before.UTC()).Scan(&coverage.Outstanding); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM x_post_versions WHERE cleanup_pending=1)`).Scan(&coverage.PendingRemoval); err != nil {
			return err
		}
		if coverage.Outstanding > 0 {
			var checked, fetched, created, suppressed sql.NullTime
			err := tx.QueryRowContext(ctx, `SELECT p.last_checked_at,p.fetched_at,p.version_created_at,p.suppressed_at
 FROM x_post_versions p WHERE p.source_id=? AND EXISTS(SELECT 1 FROM x_post_versions v
 WHERE v.source_id=p.source_id AND v.canonical_id=p.canonical_id AND `+xLatestRefreshAlias+` AND (`+xRefreshNeedsCheck+` OR v.cleanup_pending=1))
 ORDER BY COALESCE(p.last_checked_at,p.fetched_at,p.version_created_at,p.suppressed_at) ASC NULLS LAST LIMIT 1`, sourceID, before.UTC()).Scan(&checked, &fetched, &created, &suppressed)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			for _, value := range []sql.NullTime{checked, fetched, created, suppressed} {
				if value.Valid {
					coverage.OldestUncheckedAt = value.Time.UTC()
					break
				}
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT v.post_id FROM x_post_versions v WHERE v.source_id=? AND `+xLatestRefreshAlias+` AND `+xRefreshNeedsCheck+`
 ORDER BY v.last_checked_at ASC NULLS FIRST,LENGTH(v.post_id),v.post_id LIMIT ?`, sourceID, before.UTC(), limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return ids, coverage, err
}

// MarkXPostsUnverifiedContext records uncertainty before actual admitted lookup
// I/O. It changes no text or check timestamp and never touches unrequested IDs.
func (s *Store) MarkXPostsUnverifiedContext(ctx context.Context, sourceID int64, ids []string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if sourceID <= 0 || len(ids) == 0 || len(ids) > 100 {
		return errors.New("invalid X lookup observation")
	}
	args := []any{sourceID, sourceID}
	for _, id := range ids {
		if !validXControlID(id) {
			return errors.New("invalid X lookup observation")
		}
		args = append(args, id)
	}
	return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
			return err
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		_, err := tx.ExecContext(ctx, `UPDATE x_post_versions SET availability='unverified'
 WHERE source_id=? AND suppressed_at IS NULL AND canonical_id IN
 (SELECT canonical_id FROM x_post_versions WHERE source_id=? AND post_id IN (`+marks+`))`, args...)
		return err
	})
}

// QueueXAuthorRemovalContext durably queues every known chain for a confirmed
// protected numeric author across X sources. The selected writable source is
// the observation's authority, including its sync-generation guard. This only
// queues suppression; the owner coordinator purges each source after acquiring
// its execution, outside the observing source's execution and SQL transaction.
func (s *Store) QueueXAuthorRemovalContext(ctx context.Context, sourceID int64, authorID string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if sourceID <= 0 || !validXControlID(authorID) {
		return errors.New("invalid X author removal")
	}
	return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id=?`, sourceID).Scan(&kind); err != nil {
			return err
		}
		if kind != "x" {
			return errors.New("X author removal requires an X source")
		}
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE x_post_versions SET suppressed_at=COALESCE(suppressed_at,`+s.dialect.Now()+`),
 suppression_reason='protected',availability='protected',cleanup_pending=1
 WHERE (suppressed_at IS NULL OR cleanup_pending=1) AND (source_id,canonical_id) IN
 (SELECT v.source_id,v.canonical_id FROM x_post_versions v WHERE v.author_id=?
 AND EXISTS(SELECT 1 FROM sources src WHERE src.id=v.source_id AND src.source_type='x'))`, authorID)
		return err
	})
}

// CompletedXRemovalsContext identifies terminal obligations only after the
// archive-wide derived cleanup fence has cleared. It reads no message content.
func (s *Store) CompletedXRemovalsContext(ctx context.Context, sourceID int64, ids []string) ([]string, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return nil, err
	}
	if sourceID <= 0 || len(ids) > XMaxUnresolvedIDs {
		return nil, errors.New("invalid X terminal coverage query")
	}
	var completed []string
	for offset := 0; offset < len(ids); offset += 100 {
		batch := ids[offset:min(offset+100, len(ids))]
		args := []any{sourceID}
		for _, id := range batch {
			if !validXControlID(id) {
				return nil, errors.New("invalid X terminal coverage query")
			}
			args = append(args, id)
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		query := `SELECT post_id FROM x_post_versions WHERE source_id=? AND post_id IN (` + marks + `) AND suppressed_at IS NOT NULL AND cleanup_pending=0 AND NOT EXISTS(SELECT 1 FROM x_post_versions p WHERE p.cleanup_pending=1)`
		rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			completed = append(completed, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return completed, nil
}
