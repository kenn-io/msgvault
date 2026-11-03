package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/activitydate"
	"go.kenn.io/msgvault/internal/archivemarker"
)

// MessageDisplay carries scoped metadata for the text and anchor of an entry.
type MessageDisplay struct {
	MessageID                int64
	Subject, Snippet         string
	MessageType              string
	HiddenText, Encrypted    bool
	OccurredAt               *time.Time
	ConversationID, SourceID int64
	CopyKey                  string `json:"-"`
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
		eligible := strings.Replace(s.activityLiveEligibleSQL(), " AND m.deleted_from_source_at IS NULL\n", "\n", 1)
		eligible = strings.Replace(eligible, "COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL", "TRUE", 1)
		channel := `CASE WHEN m.message_type IN ('calendar_event','meeting_transcript') THEN 'meeting' WHEN m.message_type='call_record' THEN 'chat' WHEN c.conversation_type='email_thread' THEN 'email' WHEN c.conversation_type IN ('chat','group_chat','direct_chat','channel') THEN 'chat' ELSE 'other' END`
		copyKey := `CASE WHEN ` + channel + `='email' THEN ` + s.activityCopyKeySQL(channel, "m.rfc822_message_id", "m.id") + ` ELSE '' END`
		rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT m.id,COALESCE(m.subject,''),COALESCE(m.snippet,''),m.hidden_text_present,
   m.sent_at,m.received_at,m.internal_date,COALESCE(m.conversation_id,0),m.source_id,src.source_type,m.message_type,`+copyKey+`
   FROM messages m JOIN sources src ON src.id=m.source_id LEFT JOIN conversations c ON c.id=m.conversation_id WHERE m.id IN (`+marks+`) AND `+eligible+scope), args...)
		if err != nil {
			return nil, fmt.Errorf("read message display: %w", err)
		}
		for rows.Next() {
			var d MessageDisplay
			var sent, received, internal nullableTimestamp
			var sourceType string
			if err = rows.Scan(&d.MessageID, &d.Subject, &d.Snippet, &d.HiddenText, &sent, &received, &internal, &d.ConversationID, &d.SourceID, &sourceType, &d.MessageType, &d.CopyKey); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan message display: %w", err)
			}
			date, dateErr := activitydate.DateEvent(activitydate.Timestamps{SentAt: nullableTimePointer(sent), ReceivedAt: nullableTimePointer(received), InternalDate: nullableTimePointer(internal)}, time.UTC)
			if dateErr == nil {
				d.OccurredAt = &date.OccurredAt
			}
			d.Encrypted = sourceType == "matrix" && d.Snippet == archivemarker.EncryptedMessagePlaceholder
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
