package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/imapconfig"
)

// Use the native client parser and canonical identity, including effective
// port/transport/auth defaults, so equivalent configuration preserves refs.
func sameMCPIMAPRoute(before, after string) bool {
	old, oldErr := imapconfig.ConfigFromJSON(before)
	next, nextErr := imapconfig.ConfigFromJSON(after)
	if oldErr != nil || nextErr != nil {
		return before == after
	}
	return old.Identifier() == next.Identifier() && old.EffectiveAuthMethod() == next.EffectiveAuthMethod()
}

func (s *Store) invalidateMCPIMAPConfigTx(ctx context.Context, tx *loggedTx, sourceID int64, next string) error {
	var kind string
	var before sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT source_type,sync_config FROM sources WHERE id=?`, sourceID).Scan(&kind, &before)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read source routing configuration: %w", err)
	}
	if kind != sourceTypeIMAP || sameMCPIMAPRoute(before.String, next) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_source_resources WHERE source_id=?`, sourceID); err != nil {
		return fmt.Errorf("invalidate source routing lifetime: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_conversation_resources WHERE source_id=?`, sourceID); err != nil {
		return fmt.Errorf("invalidate source conversation lifetimes: %w", err)
	}
	return nil
}
