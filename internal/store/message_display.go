package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/activitydate"
)

// MessageDisplay carries scoped metadata for the text and anchor of an entry.
type MessageDisplay struct {
	MessageID                int64
	Subject, Snippet         string
	MessageType              string
	HiddenText, Encrypted    bool
	OccurredAt               *time.Time
	ConversationID, SourceID int64
}

func (s *Store) MessageDisplayContext(ctx context.Context, ids []int64) (map[int64]MessageDisplay, error) {
	result := make(map[int64]MessageDisplay, len(ids))
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	for start := 0; start < len(ids); start += activityCandidateIDChunk {
		batch := ids[start:min(start+activityCandidateIDChunk, len(ids))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch)+len(scopeArgs))
		for _, id := range batch {
			args = append(args, id)
		}
		args = append(args, scopeArgs...)
		// Undated anchors still carry safety and identity metadata.
		eligible := s.activityLiveEligibleWithoutDateSQL()
		rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT m.id,COALESCE(m.subject,''),COALESCE(m.snippet,''),m.hidden_text_present,
   m.sent_at,m.received_at,m.internal_date,COALESCE(m.conversation_id,0),m.source_id,`+matrixPendingEncryptedSQL("m")+`,m.message_type
   FROM messages m JOIN sources src ON src.id=m.source_id WHERE m.id IN (`+marks+`) AND (`+eligible+`)`+scope), args...)
		if err != nil {
			return nil, fmt.Errorf("read message display: %w", err)
		}
		for rows.Next() {
			var d MessageDisplay
			var sent, received, internal nullableTimestamp
			if err = rows.Scan(&d.MessageID, &d.Subject, &d.Snippet, &d.HiddenText, &sent, &received, &internal, &d.ConversationID, &d.SourceID, &d.Encrypted, &d.MessageType); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan message display: %w", err)
			}
			date, dateErr := activitydate.DateEvent(activitydate.Timestamps{SentAt: nullableTimePointer(sent), ReceivedAt: nullableTimePointer(received), InternalDate: nullableTimePointer(internal)}, time.UTC)
			if dateErr == nil {
				d.OccurredAt = &date.OccurredAt
			}
			if d.HiddenText || d.Encrypted {
				d.Snippet = ""
			}
			result[d.MessageID] = d
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("iterate message display: %w", err)
		}
	}
	return result, nil
}
