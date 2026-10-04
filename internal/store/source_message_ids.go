package store

import (
	"context"
	"errors"
	"fmt"
)

// SourceMessageIdentity identifies an archived message without loading its body.
type SourceMessageIdentity struct {
	ID              int64
	SourceMessageID string
}

// ListSourceMessageIDsPageContext returns a bounded keyset page for source
// artifact reconciliation. Slow provider work happens after the cursor closes.
func (s *Store) ListSourceMessageIDsPageContext(ctx context.Context, sourceID, afterID int64, limit int) ([]SourceMessageIdentity, error) {
	if sourceID <= 0 || afterID < 0 || limit <= 0 || limit > 1000 {
		return nil, errors.New("invalid source message page bounds")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_message_id FROM messages WHERE source_id = ? AND id > ? ORDER BY id LIMIT ?`, sourceID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list source message identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []SourceMessageIdentity
	for rows.Next() {
		var row SourceMessageIdentity
		if err := rows.Scan(&row.ID, &row.SourceMessageID); err != nil {
			return nil, fmt.Errorf("scan source message identity: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
