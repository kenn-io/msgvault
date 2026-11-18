package store

import (
	"context"
	"fmt"
)

const embeddingRevisionBaselineBatchSize int64 = 5000

// backfillMessageEmbeddingRevisionBaseline hides pre-revision vectors for
// messages whose old embed_gen watermark was already NULL. It walks in
// bounded, committed batches so a cancelled schema upgrade can resume safely.
func (s *Store) backfillMessageEmbeddingRevisionBaseline(ctx context.Context) error {
	firstBatchSQL := s.dialect.Rebind(`
		SELECT id FROM messages
		WHERE embed_gen IS NULL AND embedding_revision = 0
		ORDER BY id LIMIT ?`)
	nextBatchSQL := s.dialect.Rebind(`
		SELECT id FROM messages
		WHERE embed_gen IS NULL AND embedding_revision = 0 AND id > ?
		ORDER BY id LIMIT ?`)
	stampSQL := s.dialect.Rebind(`
		UPDATE messages SET embedding_revision = 1
		WHERE embed_gen IS NULL AND embedding_revision = 0 AND id >= ? AND id <= ?`)

	var cursor int64
	for batch := 0; ; batch++ {
		var firstID, lastID, count int64
		if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			var err error
			firstID, lastID, count, err = nextEmbeddingRevisionBaselineBatch(
				ctx, tx, firstBatchSQL, nextBatchSQL, batch == 0, cursor,
			)
			if err != nil || count == 0 {
				return err
			}
			if _, err := tx.ExecContext(ctx, stampSQL, firstID, lastID); err != nil {
				return fmt.Errorf("stamp messages %d through %d: %w", firstID, lastID, err)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("batch above id %d: %w", cursor, err)
		}
		if count == 0 {
			return nil
		}
		cursor = lastID
	}
}

func nextEmbeddingRevisionBaselineBatch(
	ctx context.Context,
	tx *loggedTx,
	firstBatchSQL, nextBatchSQL string,
	first bool,
	afterID int64,
) (firstID, lastID, count int64, err error) {
	var rows *loggedRows
	if first {
		rows, err = tx.QueryContext(ctx, firstBatchSQL, embeddingRevisionBaselineBatchSize)
	} else {
		rows, err = tx.QueryContext(ctx, nextBatchSQL, afterID, embeddingRevisionBaselineBatchSize)
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("select unstamped embedding revisions above id %d: %w", afterID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, 0, 0, fmt.Errorf("scan unstamped embedding revision: %w", err)
		}
		if count == 0 {
			firstID = id
		}
		lastID = id
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("read unstamped embedding revisions above id %d: %w", afterID, err)
	}
	return firstID, lastID, count, nil
}
