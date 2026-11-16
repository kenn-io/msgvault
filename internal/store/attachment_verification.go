package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AttachmentVerificationRow is a bounded copy of an occurrence's physical
// references. Empty hashes and provider outcomes are retained, not discarded
// as though the occurrence had been verified. No personal message text is read.
type AttachmentVerificationRow struct {
	ID            int64
	MessageID     int64
	ContentHash   string
	StoragePath   string
	Size          sql.NullInt64
	ThumbnailHash string
	ThumbnailPath string
	State         string
	SkipReason    string
}

// SelectAttachmentVerificationRows reads one ordered page and closes its SQL
// cursor before returning. Every retained attachment is included, including
// occurrences on hidden/deleted messages and preserved source-merge rows.
// Strings are capped in SQL so malformed metadata can be reported without
// an unbounded allocation. There is no file I/O here.
func (s *Store) SelectAttachmentVerificationRows(ctx context.Context, afterID int64, limit int) ([]AttachmentVerificationRow, bool, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return nil, false, errors.New("attachment verification requires after-id >= 0 and limit between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, message_id,
  substr(COALESCE(content_hash, ''), 1, 65), substr(COALESCE(storage_path, ''), 1, 4097), size,
  substr(COALESCE(thumbnail_hash, ''), 1, 65), substr(COALESCE(thumbnail_path, ''), 1, 4097),
  substr(COALESCE(attachment_state, ''), 1, 65), substr(COALESCE(attachment_skip_reason, ''), 1, 65)
  FROM attachments WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("select attachment verification references: %w", err)
	}
	selected := make([]AttachmentVerificationRow, 0, limit)
	for rows.Next() {
		var row AttachmentVerificationRow
		if err := rows.Scan(&row.ID, &row.MessageID, &row.ContentHash, &row.StoragePath, &row.Size,
			&row.ThumbnailHash, &row.ThumbnailPath, &row.State, &row.SkipReason); err != nil {
			return nil, false, errors.Join(err, rows.Close())
		}
		selected = append(selected, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	more := len(selected) > limit
	if more {
		selected = selected[:limit]
	}
	return selected, more, nil
}
