package store

import (
	"context"
	"database/sql"
	"fmt"
)

const accountCatalogRevisionKey = "account_catalog_revision"

// AccountCatalogRevisionContext returns the revision an account catalog read
// depends on. It advances when an identity is confirmed or removed, and when a
// committed transaction rewrote any message's account attribution, whichever
// process committed it: the catalog counts rows by attribution, so a read from
// before either change is stale. It sums two counters that only increase, and
// never invalidates analytics caches, which keep using AccountIdentityRevision.
func (s *Store) AccountCatalogRevisionContext(ctx context.Context) (int64, error) {
	var revision int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(CAST(value AS INTEGER)), 0) FROM archive_metadata WHERE key IN (?, ?)`,
		accountIdentityRevisionKey, accountCatalogRevisionKey,
	).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read account catalog revision: %w", err)
	}
	return revision, nil
}

// attributionWriter routes the statements execInChunksContext issues through
// execAttributionContext.
type attributionWriter struct{ *loggedTx }

func (w attributionWriter) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return w.execAttributionContext(ctx, query, args...)
}

// execAttributionContext runs a statement that rewrites messages.account_address
// or account_path and remembers that it changed rows, so Commit bumps the
// catalog revision once however many rows or statements the transaction wrote.
func (t *loggedTx) execAttributionContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := t.ExecContext(ctx, query, args...)
	if err == nil {
		if n, rowsErr := res.RowsAffected(); rowsErr != nil || n > 0 {
			t.accountCatalogChanged = true
		}
	}
	return res, err
}

// Commit bumps the account catalog revision in the same transaction when it
// rewrote attribution, so the revision and the rows change together.
func (t *loggedTx) Commit() error {
	if t.accountCatalogChanged {
		t.accountCatalogChanged = false
		if _, err := t.ExecContext(t.ctx, `
			INSERT INTO archive_metadata (key, value) VALUES (?, '1')
			ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(archive_metadata.value AS INTEGER) + 1 AS TEXT)`,
			accountCatalogRevisionKey); err != nil {
			_ = t.Rollback()
			return fmt.Errorf("bump account catalog revision: %w", err)
		}
	}
	return t.Tx.Commit()
}
