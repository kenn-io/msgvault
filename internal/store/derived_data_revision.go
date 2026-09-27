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

func (s *Store) bumpDerivedDataRevision(tx *loggedTx) error {
	if _, err := tx.Exec(s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		derivedDataRevisionKey); err != nil {
		return fmt.Errorf("seed derived-data revision: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE archive_metadata
		SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		WHERE key = ?
	`, derivedDataRevisionKey); err != nil {
		return fmt.Errorf("bump derived-data revision: %w", err)
	}
	return nil
}

// AdvanceDerivedDataRevision records a repair attempt that may have committed
// changes but was not complete enough to enter the migration ledger. The next
// cache maintenance pass must still publish those partial, authoritative rows.
func (s *Store) AdvanceDerivedDataRevision() error {
	return s.withTx(func(tx *loggedTx) error {
		return s.bumpDerivedDataRevision(tx)
	})
}

// MarkMigrationAppliedWithDerivedDataRevision atomically records a completed
// re-derivation and advances the cache-visible revision. The ledger can never
// claim a repair is complete without also making an older analytics cache
// stale.
func (s *Store) MarkMigrationAppliedWithDerivedDataRevision(name string) error {
	return s.withTx(func(tx *loggedTx) error {
		if err := s.bumpDerivedDataRevision(tx); err != nil {
			return err
		}
		return s.markMigrationAppliedContext(context.Background(), tx, name, 1)
	})
}
