package store

import (
	"context"
	"fmt"
)

// AgentAttachmentSourceIDsContext resolves attachment authority through its
// containing messages. A shared blob may be readable from several sources.
func (s *Store) AgentAttachmentSourceIDsContext(ctx context.Context, id int64, hash string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id FROM sources s WHERE EXISTS (
 SELECT 1 FROM attachments a JOIN messages m ON m.id=a.message_id
 WHERE m.source_id=s.id AND ((? > 0 AND a.id=?) OR (? <> '' AND a.content_hash=?)))`, id, id, hash, hash)
	if err != nil {
		return nil, fmt.Errorf("resolve attachment sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan attachment source: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
