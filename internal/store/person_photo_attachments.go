package store

import (
	"context"
	"fmt"
	"strings"
)

// PersonPhotoOccurrences maps exact versions to live image attachments. The
// image filter precedes deduplication so a non-image label cannot hide an image.
func (s *Store) PersonPhotoOccurrences(ctx context.Context, destination string, versionIDs []string, limit int) ([]DocumentSearchOccurrence, error) {
	var result []DocumentSearchOccurrence
	if len(versionIDs) == 0 {
		return result, nil
	}
	if len(versionIDs) > 100 || limit < 1 || limit > 10 {
		return nil, fmt.Errorf("invalid photo occurrence bounds")
	}
	args := make([]any, len(versionIDs))
	for i, id := range versionIDs {
		args[i] = id
	}
	with, query, values := s.documentSearchRows(destination, nil, `d.version_id IN (?`+strings.Repeat(",?", len(versionIDs)-1)+`) AND LOWER(COALESCE(a.mime_type,'')) LIKE 'image/%'`, args)
	query = with + `, duplicates AS (
 SELECT d.version_id, d.attachment_id, m.id AS message_id, cv.id AS conversation_id,
 d.filename, ` + containingTitleMCVSQL + ` AS containing_title,
 COALESCE(m.sent_at,m.received_at,m.internal_date) AS occurred_at,
 ROW_NUMBER() OVER (PARTITION BY d.version_id, m.id ORDER BY d.attachment_id) AS duplicate_rank` + query + `
 ), ranked AS (SELECT *,
 ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY occurred_at DESC NULLS LAST, attachment_id) AS occurrence_rank FROM duplicates WHERE duplicate_rank = 1
 ) SELECT version_id, attachment_id, message_id, conversation_id, filename, containing_title, occurred_at
 FROM ranked WHERE occurrence_rank <= ? ORDER BY occurred_at DESC NULLS LAST, attachment_id`
	values = append(values, limit+1)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), values...)
	if err != nil {
		return nil, fmt.Errorf("list person photo occurrences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o DocumentSearchOccurrence
		var date nullableTimestamp
		if err := rows.Scan(&o.VersionID, &o.AttachmentID, &o.MessageID, &o.ConversationID, &o.Filename, &o.ContainingTitle, &date); err != nil {
			return nil, fmt.Errorf("scan person photo occurrence: %w", err)
		}
		o.OccurredAt = optionalTimestamp(date)
		result = append(result, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person photo occurrences: %w", err)
	}
	return result, nil
}
