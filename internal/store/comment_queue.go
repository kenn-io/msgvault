package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CommentTriageState string

const (
	CommentTriageOpen    CommentTriageState = "open"
	CommentTriageHandled CommentTriageState = "handled"
	CommentTriageSkipped CommentTriageState = "skipped"
)

var (
	ErrCommentTriageConflict = errors.New("comment triage state changed")
	ErrCommentTriageInvalid  = errors.New("invalid comment triage target or state")
)

const commentTriageSQL = `CASE WHEN EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND l.source_id=m.source_id AND l.source_label_id='msgvault:triage:handled') THEN 'handled'
 WHEN EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND l.source_id=m.source_id AND l.source_label_id='msgvault:triage:skipped') THEN 'skipped' ELSE 'open' END`

func validCommentTriage(state CommentTriageState) bool {
	return state == CommentTriageOpen || state == CommentTriageHandled || state == CommentTriageSkipped
}
func (s *Store) CommentTriageContext(ctx context.Context, id int64) (state CommentTriageState, err error) {
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT `+commentTriageSQL+` FROM messages m WHERE m.id=? AND m.deleted_at IS NULL AND m.reply_to_message_id IS NOT NULL AND m.message_type IN ('facebook_comments','instagram_comments')`), id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrCommentTriageInvalid
	}
	return
}
func (s *Store) SetCommentTriageContext(ctx context.Context, id int64, state CommentTriageState, expect *CommentTriageState) (CommentTriageState, error) {
	if !validCommentTriage(state) || (expect != nil && !validCommentTriage(*expect)) {
		return "", ErrCommentTriageInvalid
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		query := `SELECT m.source_id FROM messages m WHERE m.id=? AND m.deleted_at IS NULL AND m.reply_to_message_id IS NOT NULL AND m.message_type IN ('facebook_comments','instagram_comments')`
		if s.IsPostgreSQL() {
			query += ` FOR UPDATE OF m`
		}
		var sourceID int64
		var current CommentTriageState
		if err := tx.QueryRowContext(ctx, s.Rebind(query), id).Scan(&sourceID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCommentTriageInvalid
			}
			return err
		}
		// A new statement after the row lock sees labels committed by a
		// previous owner while our lock acquisition was waiting.
		if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT `+commentTriageSQL+` FROM messages m WHERE m.id=?`), id).Scan(&current); err != nil {
			return err
		}
		if expect != nil && current != *expect {
			return ErrCommentTriageConflict
		}
		if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM message_labels WHERE message_id=? AND label_id IN (SELECT id FROM labels WHERE source_id=? AND source_label_id IN ('msgvault:triage:handled','msgvault:triage:skipped'))`), id, sourceID); err != nil {
			return err
		}
		if state == CommentTriageOpen {
			return nil
		}
		name := "Handled"
		if state == CommentTriageSkipped {
			name = "Skipped"
		}
		label, err := ensureLabelWith(tx, sourceID, "msgvault:triage:"+string(state), name, "user", nil)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO message_labels(message_id,label_id) VALUES (?,?) ON CONFLICT DO NOTHING`), id, label)
		return err
	})
	return state, err
}

type CommentQueueFilter struct {
	MessageTypes []string
	SourceID     int64
	State        string
	Limit        int
	BeforeID     int64
}
type CommentQueueItem struct {
	MessageID, SourceID, ConversationID      int64
	SourceIdentifier, MessageType, CommentID string
	ParentMessageID, AuthorParticipantID     int64
	AuthorName, Snippet                      string
	SentAt                                   *time.Time
	Metadata                                 string
	Triage                                   CommentTriageState
	DeletedFromSource, PageReplied           bool
	PrivateReplyOpenUntil                    *time.Time
}

// BeforeID is the last item seen in this ascending queue. Its timestamp and ID
// form the continuation position, even when ingestion IDs are out of time order.
func (s *Store) ListCommentQueueContext(ctx context.Context, f CommentQueueFilter) ([]CommentQueueItem, error) {
	if f.State == "" {
		f.State = "open"
	}
	if f.Limit == 0 {
		f.Limit = 50
	}
	if (!validCommentTriage(CommentTriageState(f.State)) && f.State != "all") || f.Limit < 1 || f.Limit > 200 || f.BeforeID < 0 || f.SourceID < 0 {
		return nil, ErrCommentTriageInvalid
	}
	if len(f.MessageTypes) == 0 {
		f.MessageTypes = []string{"facebook_comments"}
	}
	placeholders := make([]string, len(f.MessageTypes))
	args := []any{}
	for i, typ := range f.MessageTypes {
		if typ != "facebook_comments" && typ != "instagram_comments" {
			return nil, ErrCommentTriageInvalid
		}
		placeholders[i] = "?"
		args = append(args, typ)
	}
	query := `WITH queue AS (SELECT m.id AS message_id,m.source_id,m.conversation_id,s.identifier,m.message_type,m.source_message_id,m.reply_to_message_id,COALESCE(m.sender_id,0),COALESCE(p.display_name,''),COALESCE(m.snippet,''),m.sent_at,COALESCE(CAST(m.metadata AS TEXT),''),` + commentTriageSQL + ` AS triage,m.deleted_from_source_at IS NOT NULL,
 EXISTS (SELECT 1 FROM messages child WHERE child.reply_to_message_id=m.id AND child.source_id=m.source_id AND child.is_from_me=TRUE AND child.deleted_at IS NULL),
 EXISTS (SELECT 1 FROM meta_comment_actions a WHERE a.source_id=m.source_id AND a.target_comment_id=m.source_message_id AND a.action='private_reply' AND a.state<>'failed')
 FROM messages m JOIN sources s ON s.id=m.source_id LEFT JOIN participants p ON p.id=m.sender_id
 WHERE m.deleted_at IS NULL AND m.reply_to_message_id IS NOT NULL AND m.is_from_me=FALSE AND m.message_type IN (` + strings.Join(placeholders, ",") + `)`
	if f.SourceID != 0 {
		query += ` AND m.source_id=?`
		args = append(args, f.SourceID)
	}
	if f.State != "all" {
		query += ` AND m.deleted_from_source_at IS NULL`
	}
	if f.BeforeID != 0 {
		query += ` AND EXISTS (SELECT 1 FROM messages cursor WHERE cursor.id=? AND ((cursor.sent_at IS NULL AND m.sent_at IS NOT NULL) OR m.sent_at>cursor.sent_at OR ((m.sent_at=cursor.sent_at OR (m.sent_at IS NULL AND cursor.sent_at IS NULL)) AND m.id>cursor.id)))`
		args = append(args, f.BeforeID)
	}
	query += `) SELECT * FROM queue`
	if f.State != "all" {
		query += ` WHERE triage=?`
		args = append(args, f.State)
	}
	query += ` ORDER BY CASE WHEN sent_at IS NULL THEN 0 ELSE 1 END,sent_at,message_id LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list comment queue: %w", err)
	}
	defer rows.Close()
	result := []CommentQueueItem{}
	for rows.Next() {
		var item CommentQueueItem
		var sent sql.NullTime
		var privateSent bool
		if err := rows.Scan(&item.MessageID, &item.SourceID, &item.ConversationID, &item.SourceIdentifier, &item.MessageType, &item.CommentID, &item.ParentMessageID, &item.AuthorParticipantID, &item.AuthorName, &item.Snippet, &sent, &item.Metadata, &item.Triage, &item.DeletedFromSource, &item.PageReplied, &privateSent); err != nil {
			return nil, err
		}
		if sent.Valid {
			item.SentAt = &sent.Time
			if !privateSent {
				until := sent.Time.Add(7 * 24 * time.Hour)
				item.PrivateReplyOpenUntil = &until
			}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
