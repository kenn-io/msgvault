package store

import (
	"context"
	"fmt"
)

// bumpInboxSourceRevisionTx advances the source observation revision in the
// same transaction as its first working consumer, provider-state persistence.
func bumpInboxSourceRevisionTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO inbox_source_revisions(source_id,revision) VALUES (?,1)
  ON CONFLICT(source_id) DO UPDATE SET revision=inbox_source_revisions.revision+1`, sourceID)
	if err != nil {
		return fmt.Errorf("advance inbox archive revision: %w", err)
	}
	return nil
}
