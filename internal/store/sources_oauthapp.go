package store

import (
	"context"
	"database/sql"
	"fmt"
)

// UpdateSourceOAuthApp updates the OAuth app binding for a source.
// Pass a null NullString to clear the binding (use default app).
func (s *Store) UpdateSourceOAuthApp(sourceID int64, oauthApp sql.NullString) error {
	ctx := context.Background()
	return s.withIdentityTxContext(ctx, nil, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE sources
		SET oauth_app = ?, updated_at = %s
		WHERE id = ?
	`, s.dialect.Now()), oauthApp, sourceID)
		return err
	})
}
