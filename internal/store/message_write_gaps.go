package store

import (
	"context"
	"fmt"
)

// MessageWriteGaps reports, for one archived message, which of the writes a
// completed import always leaves behind are missing. Importers whose message
// writes commit in several steps use it to converge messages an interruption
// left half-written: each absent write is refilled from the source's current
// bytes without overwriting values already present.
type MessageWriteGaps struct {
	// BodyMissing reports that message_bodies carries no row.
	BodyMissing bool
	// RecipientsMissing reports that no message_recipients row exists.
	RecipientsMissing bool
	// SearchMissing reports that the message has no search document — no
	// messages_fts row on SQLite, a NULL search_fts on PostgreSQL.
	SearchMissing bool
	// ReplyLinkMissing reports that reply_to_message_id is still NULL.
	ReplyLinkMissing bool
	// Edited reports the stored is_edited flag, so a caller that observes
	// an edit marker can restore a flag the first write lost.
	Edited bool
}

// MessageWriteGapsContext reports the missing deferred writes for the given
// message ids. Ids that do not exist are absent from the result.
func (s *Store) MessageWriteGapsContext(ctx context.Context, messageIDs []int64) (map[int64]MessageWriteGaps, error) {
	result := make(map[int64]MessageWriteGaps, len(messageIDs))
	if len(messageIDs) == 0 {
		return result, nil
	}
	searchAbsent := "FALSE" // without the FTS5 module there is no document to miss
	if s.IsPostgreSQL() {
		searchAbsent = "m.search_fts IS NULL"
	} else if s.fts5Available {
		searchAbsent = "NOT EXISTS (SELECT 1 FROM messages_fts f WHERE f.rowid = m.id)"
	}
	err := queryInChunksContext(ctx, s.db, messageIDs, nil, `
		SELECT m.id,
		       NOT EXISTS (SELECT 1 FROM message_bodies b WHERE b.message_id = m.id),
		       NOT EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id = m.id),
		       `+searchAbsent+`,
		       m.reply_to_message_id IS NULL,
		       COALESCE(m.is_edited, FALSE)
		FROM messages m
		WHERE m.id IN (%s)`, func(rows *loggedRows) error {
		var id int64
		var gaps MessageWriteGaps
		if err := rows.Scan(&id, &gaps.BodyMissing, &gaps.RecipientsMissing,
			&gaps.SearchMissing, &gaps.ReplyLinkMissing, &gaps.Edited); err != nil {
			return err
		}
		result[id] = gaps
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("message write gaps: %w", err)
	}
	return result, nil
}
