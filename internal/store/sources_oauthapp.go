package store

import (
	"database/sql"
	"fmt"
)

// UpdateSourceOAuthApp updates the OAuth app binding for a source.
// Pass a null NullString to clear the binding (use default app).
func (s *Store) UpdateSourceOAuthApp(sourceID int64, oauthApp sql.NullString) error {
	return s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		_, err := tx.Exec(fmt.Sprintf(`
		UPDATE sources
		SET oauth_app = ?, updated_at = %s
		WHERE id = ?
	`, s.dialect.Now()), oauthApp, sourceID)
		return err
	})
}
