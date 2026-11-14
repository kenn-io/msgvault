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

// UpdateSourceO365Registration commits the Microsoft mail binding and IMAP settings together.
func (s *Store) UpdateSourceO365Registration(ctx context.Context, sourceID int64, identifier string, oauthApp sql.NullString, syncConfig string) error {
	var pending []int64
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		pending = nil
		if err := s.updateSourceIdentifierTx(ctx, tx, sourceID, identifier, &pending); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE sources SET sync_config = %s, oauth_app = ?, updated_at = %s WHERE id = ?`, s.dialect.JSONBindExpr(), s.dialect.Now()), syncConfig, oauthApp, sourceID)
		return err
	})
	if err == nil {
		s.deriveAccountAttributionAfterCommit(ctx, sourceID, pending)
	}
	return err
}
