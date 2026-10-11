package store

import (
	"context"
	"fmt"
)

const derivedDataRevisionKey = "derived_data_revision"

// DerivedDataRevision returns the revision of existing message facts changed
// by offline re-derivation. Analytics caches stamp this value when they export
// message and attachment Parquet; a mismatch requires a full rebuild because
// incremental publication cannot rewrite already-exported rows.
func (s *Store) DerivedDataRevision() (int64, error) {
	return s.DerivedDataRevisionContext(context.Background())
}

// DerivedDataRevisionContext is the request-aware form of DerivedDataRevision.
func (s *Store) DerivedDataRevisionContext(ctx context.Context) (int64, error) {
	return readArchiveMetadataRevisionContext(ctx, s.db, derivedDataRevisionKey, "derived-data")
}

func (s *Store) bumpDerivedDataRevision(tx *loggedTx, relatedOnly ...bool) error {
	return s.bumpDerivedDataRevisionContext(context.Background(), tx, relatedOnly...)
}

// bumpDerivedDataRevisionContext honors cancellation during bumpDerivedDataRevision.
// tx may be a transaction or an exclusive connection that has begun one.
func (s *Store) bumpDerivedDataRevisionContext(ctx context.Context, tx contextStatementQuerier, relatedOnly ...bool) error {
	if _, err := tx.ExecContext(ctx, s.Rebind(s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`)),
		derivedDataRevisionKey); err != nil {
		return fmt.Errorf("seed derived-data revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.Rebind(`
		UPDATE archive_metadata
		SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		WHERE key = ?
	`), derivedDataRevisionKey); err != nil {
		return fmt.Errorf("bump derived-data revision: %w", err)
	}
	if len(relatedOnly) > 0 && relatedOnly[0] && !s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_related_revision_journal (revision)
			SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = ?`,
			derivedDataRevisionKey); err != nil {
			return fmt.Errorf("record related derived-data revision: %w", err)
		}
	}
	return nil
}

// RelatedDerivedRevisionsOnly verifies that every derived revision after the
// committed cache marker was caused by a journaled child-row mutation.
func (s *Store) RelatedDerivedRevisionsOnly(ctx context.Context, previous, current int64) (bool, error) {
	if current <= previous || s.IsPostgreSQL() {
		return false, nil
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_related_revision_journal
		WHERE revision > ? AND revision <= ?`, previous, current).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect related derived-data revisions: %w", err)
	}
	return count == current-previous, nil
}

// AdvanceDerivedDataRevision records a repair attempt that may have committed
// changes but was not complete enough to enter the migration ledger. The next
// cache maintenance pass must still publish those partial, authoritative rows.
func (s *Store) AdvanceDerivedDataRevision() error {
	return s.AdvanceDerivedDataRevisionContext(context.Background())
}

// AdvanceDerivedDataRevisionContext records partial repairs within ctx.
func (s *Store) AdvanceDerivedDataRevisionContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	})
}

// MarkMigrationAppliedWithDerivedDataRevision atomically records a completed
// re-derivation and advances the cache-visible revision. The ledger can never
// claim a repair is complete without also making an older analytics cache
// stale.
func (s *Store) MarkMigrationAppliedWithDerivedDataRevision(name string) error {
	return s.MarkMigrationAppliedWithDerivedDataRevisionContext(context.Background(), name)
}

// MarkMigrationAppliedWithDerivedDataRevisionContext records a completed repair within ctx.
func (s *Store) MarkMigrationAppliedWithDerivedDataRevisionContext(ctx context.Context, name string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
			return err
		}
		return s.markMigrationAppliedContext(ctx, tx, name, 1)
	})
}
