package store

import (
	"context"
	"database/sql"
	"fmt"
)

// UpdateSourceOAuthApp updates the OAuth app binding for a source.
// Pass a null NullString to clear the binding (use default app).
func (s *Store) UpdateSourceOAuthApp(ctx context.Context, sourceID int64, oauthApp sql.NullString) error {
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE sources
		SET oauth_app = ?, updated_at = %s
		WHERE id = ?
	`, s.dialect.Now()), oauthApp, sourceID)
	return err
}

// RegisterMicrosoftMail commits the source, IMAP settings, and app binding together.
func (s *Store) RegisterMicrosoftMail(ctx context.Context, source *Source, sourceType, identifier string, oauthApp sql.NullString, syncConfig string) (*Source, error) {
	var pending []int64
	original := source
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		pending = nil
		source = original
		if source == nil {
			var err error
			source, err = s.getOrCreateSourceTx(ctx, tx, sourceType, identifier)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO collection_sources (collection_id, source_id) SELECT id, ? FROM collections WHERE name = ?`), source.ID, DefaultCollectionName); err != nil {
				return err
			}
		}
		if syncConfig != "" {
			if err := s.updateSourceIdentifierTx(ctx, tx, source.ID, identifier, &pending); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE sources SET sync_config = %s, oauth_app = ?, updated_at = %s WHERE id = ?`, s.dialect.JSONBindExpr(), s.dialect.Now()), syncConfig, oauthApp, source.ID)
			return err
		}
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, source.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE sources SET oauth_app = ?, updated_at = %s WHERE id = ?`, s.dialect.Now()), oauthApp, source.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.deriveAccountAttributionAfterCommit(ctx, source.ID, pending)
	return source, nil
}
