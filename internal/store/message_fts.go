package store

import (
	"context"
	"fmt"
	"log/slog"
)

// indexBestEffortFTSTx isolates a derived index failure from canonical writes.
// A PostgreSQL SQL error must be rolled back before the archive transaction can
// append its occurrence and commit the ready canonical snapshot.
func (s *Store) indexBestEffortFTSTx(ctx context.Context, tx *loggedTx, id int64, doc FTSDoc) error {
	const savepoint = "best_effort_fts"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("create search index savepoint: %w", err)
	}
	doc.MessageID = id
	indexErr := s.dialect.FTSUpsert(boundQuerier{ctx: ctx, q: tx}, doc)
	if indexErr != nil {
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
			return fmt.Errorf("rollback search index: index: %w; rollback: %w", indexErr, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("release search index savepoint: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if indexErr != nil {
		slog.Warn("upsert message fts failed", "message_id", id, "reason", "index_write_failed")
	}
	return nil
}
