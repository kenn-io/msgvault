package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
)

// ErrXRemovalNeedsOwner reports work that cannot finish while a callback owns
// only one source. Keep its durable pending flag and retry from the owner
// coordinator after releasing that source; never acquire nested source locks.
var ErrXRemovalNeedsOwner = errors.New("X removal needs archive-wide source ownership")

// xNativeChainCTE follows native IDs and canonical/edit aliases across X
// sources. Only content-free identity metadata crosses the source boundary.
func (s *Store) xNativeChainCTE() string {
	seeds := `SELECT CAST(value AS TEXT) FROM json_each(?)`
	if s.IsPostgreSQL() {
		seeds = `SELECT value FROM jsonb_array_elements_text(?::jsonb) AS seeds(value)`
	}
	// Expand only the connected component, using existing (source_id,post_id)
	// and (source_id,canonical_id) indexes rather than materializing the archive.
	return `WITH RECURSIVE chain(source_id,canonical_id,post_id) AS (
 SELECT v.source_id,v.canonical_id,v.post_id
 FROM sources src CROSS JOIN x_post_versions v
 WHERE src.source_type='x' AND v.source_id=src.id AND v.post_id IN (` + seeds + `)
 UNION
 SELECT v.source_id,v.canonical_id,v.post_id FROM chain c
 JOIN x_post_versions local ON local.source_id=c.source_id AND local.canonical_id=c.canonical_id
 CROSS JOIN sources src
 JOIN x_post_versions v ON v.source_id=src.id AND v.post_id=local.post_id
 WHERE src.source_type='x'
 ) `
}

func (s *Store) xGlobalSuppressionTx(ctx context.Context, tx *loggedTx, ids []string) (sql.NullTime, bool, error) {
	data, err := json.Marshal(ids)
	if err != nil {
		return sql.NullTime{}, false, err
	}
	var at sql.NullTime
	var pending bool
	// The incoming history joins all seed components. A new ID-only local
	// alias must queue cleanup if this newly learned link exposes a foreign
	// copy; the scoped writer never mutates that foreign source itself.
	err = tx.QueryRowContext(ctx, s.xNativeChainCTE()+`SELECT v.suppressed_at,
 EXISTS(SELECT 1 FROM chain c2 JOIN x_post_versions v2 ON v2.source_id=c2.source_id AND v2.post_id=c2.post_id
 WHERE v2.cleanup_pending=1 OR (v2.message_id IS NOT NULL AND v2.suppressed_at IS NULL))
 FROM chain c JOIN x_post_versions v ON v.source_id=c.source_id AND v.post_id=c.post_id
 WHERE v.suppressed_at IS NOT NULL ORDER BY v.suppressed_at LIMIT 1`, string(data)).Scan(&at, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullTime{}, false, nil
	}
	return at, pending, err
}

// XRemovalTargetsContext finds every archived occurrence of the connected
// native edit chain. Its result is a plan, rechecked after suppression commits.
// Cross-source maintenance requires an unscoped owner Store.
func (s *Store) XRemovalTargetsContext(ctx context.Context, sourceID int64, nativeID string) ([]XRemovalWork, error) {
	if s.syncGeneration != nil {
		return nil, ErrXRemovalNeedsOwner
	}
	if sourceID <= 0 || !validXControlID(nativeID) {
		return nil, errors.New("invalid X removal identity")
	}
	var kind string
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT source_type FROM sources WHERE id=?`), sourceID).Scan(&kind); err != nil {
		return nil, err
	}
	if kind != "x" {
		return nil, errors.New("X removal requires an X source")
	}
	data, err := json.Marshal([]string{nativeID})
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(s.xNativeChainCTE()+`SELECT source_id,canonical_id FROM chain GROUP BY source_id,canonical_id ORDER BY source_id,canonical_id`), string(data))
	if err != nil {
		return nil, err
	}
	var targets []XRemovalWork
	hasSource := false
	for rows.Next() {
		var work XRemovalWork
		if err := rows.Scan(&work.SourceID, &work.CanonicalID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		work.Pending = true
		targets = append(targets, work)
		hasSource = hasSource || work.SourceID == sourceID
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if !hasSource {
		targets = append(targets, XRemovalWork{SourceID: sourceID, CanonicalID: nativeID, Pending: true})
	}
	return targets, nil
}

// XRemovalExecution owns all affected source locks in the existing single
// backend group (one PG session, including with a two-connection pool).
type XRemovalExecution struct {
	store     *Store
	sourceIDs []int64
	lock      syncExecutionLock
	Targets   []XRemovalWork
}

func (e *XRemovalExecution) Release() error {
	return e.store.abandonSyncExecutionLocks(e.sourceIDs, e.lock)
}

// AcquireXRemovalExecutionContext never waits for another source execution.
// It refuses a sync-scoped view instead of silently discarding its fence.
func (s *Store) AcquireXRemovalExecutionContext(ctx context.Context, sourceID int64, nativeID string) (*XRemovalExecution, error) {
	targets, err := s.XRemovalTargetsContext(ctx, sourceID, nativeID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, target := range targets {
		ids = append(ids, target.SourceID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	lock, err := s.acquireSyncExecutionLocks(ctx, ids)
	if err != nil {
		return nil, err
	}
	execution := &XRemovalExecution{store: s, sourceIDs: ids, lock: lock, Targets: targets}
	for _, id := range ids {
		if err := s.recoverAbandonedSyncSource(ctx, id); err != nil {
			return nil, errors.Join(fmt.Errorf("recover X removal source: %w", err), execution.Release())
		}
	}
	return execution, nil
}
