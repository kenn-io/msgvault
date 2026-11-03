package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrMetaCommentActionConflict = errors.New("comment action revision or state changed")
	ErrMetaCommentActionUnknown  = errors.New("comment action outcome is unknown")
	ErrMetaCommentActionNotFound = errors.New("comment action not found")
)

type MetaCommentAction struct {
	ID, SourceID                                                                                 int64
	SourceIdentifier                                                                             string
	MessageID                                                                                    sql.NullInt64
	TargetCommentID, TargetAuthorID, Action, Body, State, ErrorCode, RemoteID, RemoteRecipientID string
	ReplyMessageID                                                                               sql.NullInt64
	ArchiveComplete                                                                              bool
	RowRevision                                                                                  int64
	CreatedAt, UpdatedAt                                                                         time.Time
	DispatchedAt                                                                                 *time.Time
}
type MetaCommentActionClaim struct {
	ActionKey                                     string
	SourceID                                      int64
	SourceIdentifier                              string
	MessageID                                     int64
	TargetCommentID, TargetAuthorID, Action, Body string
}
type MetaCommentActionOutcome struct {
	State, ErrorCode, RemoteID, RemoteRecipientID string
	ReplyMessageID                                int64
}

const metaCommentActionFailed = "failed"

// Archive evidence is derived without repairing receipts during a read.
const metaActionColumns = `a.id,a.source_id,a.source_identifier,a.message_id,a.target_comment_id,
 COALESCE(a.target_author_id,''),a.action,a.body,a.state,COALESCE(a.error_code,''),
 COALESCE(a.remote_id,''),COALESCE(a.remote_recipient_id,''),
 COALESCE(a.reply_message_id,(SELECT m.id FROM messages m WHERE m.source_id=a.source_id AND m.source_message_id=a.remote_id LIMIT 1)),
 a.row_revision,a.created_at,a.updated_at,a.dispatched_at`

func scanMetaAction(row interface{ Scan(dest ...any) error }) (a MetaCommentAction, err error) {
	err = row.Scan(&a.ID, &a.SourceID, &a.SourceIdentifier, &a.MessageID, &a.TargetCommentID, &a.TargetAuthorID, &a.Action, &a.Body, &a.State, &a.ErrorCode, &a.RemoteID, &a.RemoteRecipientID, &a.ReplyMessageID, &a.RowRevision, &a.CreatedAt, &a.UpdatedAt, &a.DispatchedAt)
	a.ArchiveComplete = a.ReplyMessageID.Valid
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrMetaCommentActionNotFound
	}
	return
}
func (s *Store) GetMetaCommentActionByKeyContext(ctx context.Context, key string) (MetaCommentAction, error) {
	return scanMetaAction(s.db.QueryRowContext(ctx, s.Rebind(`SELECT `+metaActionColumns+` FROM meta_comment_actions a WHERE a.action_key=?`), key))
}

const succeededMetaCommentDeletionQuery = `SELECT EXISTS (SELECT 1 FROM meta_comment_actions
		WHERE source_id=? AND source_identifier=? AND target_comment_id=? AND action='delete' AND state='succeeded')`

// HasSucceededMetaCommentDeletionContext retains a successful deletion's
// tombstone after GC removes the archived message and its receipt link.
func (s *Store) HasSucceededMetaCommentDeletionContext(ctx context.Context, sourceID int64, sourceIdentifier, commentID string) (bool, error) {
	var deleted bool
	err := s.db.QueryRowContext(ctx, s.Rebind(succeededMetaCommentDeletionQuery), sourceID, sourceIdentifier, commentID).Scan(&deleted)
	if err != nil {
		return false, fmt.Errorf("read deleted comment tombstone: %w", err)
	}
	return deleted, nil
}

// ClaimMetaCommentActionContext uses database uniqueness to prevent duplicate
// dispatch across daemons. No transaction outlives this call; provider I/O
// belongs to the service layer.
func (s *Store) ClaimMetaCommentActionContext(ctx context.Context, c MetaCommentActionClaim) (a MetaCommentAction, existing bool, err error) {
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var id int64
		insertErr := tx.QueryRowContext(ctx, s.Rebind(`INSERT INTO meta_comment_actions
   (action_key,source_id,source_identifier,message_id,target_comment_id,target_author_id,action,body,state)
   VALUES (?,?,?,NULLIF(?,0),?,NULLIF(?,''),?,?,'claimed') ON CONFLICT DO NOTHING RETURNING id`), c.ActionKey, c.SourceID, c.SourceIdentifier, c.MessageID, c.TargetCommentID, c.TargetAuthorID, c.Action, c.Body).Scan(&id)
		if insertErr != nil && !errors.Is(insertErr, sql.ErrNoRows) {
			return fmt.Errorf("claim comment action: %w", insertErr)
		}
		if insertErr == nil {
			var readErr error
			a, readErr = scanMetaAction(tx.QueryRowContext(ctx, s.Rebind(`SELECT `+metaActionColumns+` FROM meta_comment_actions a WHERE a.id=?`), id))
			return readErr
		}
		var readErr error
		a, readErr = scanMetaAction(tx.QueryRowContext(ctx, s.Rebind(`SELECT `+metaActionColumns+` FROM meta_comment_actions a WHERE a.action_key=? OR
   (a.source_id=? AND a.target_comment_id=? AND a.action='private_reply' AND ?='private_reply' AND a.state<>'failed') ORDER BY CASE WHEN a.state='failed' THEN 1 ELSE 0 END,a.id LIMIT 1`), c.ActionKey, c.SourceID, c.TargetCommentID, c.Action))
		if readErr != nil {
			return readErr
		}
		existing = true
		if a.State != metaCommentActionFailed {
			return nil
		}
		result, updateErr := tx.ExecContext(ctx, s.Rebind(`UPDATE meta_comment_actions SET state='claimed',error_code=NULL,remote_id=NULL,remote_recipient_id=NULL,reply_message_id=NULL,dispatched_at=NULL,row_revision=row_revision+1,updated_at=? WHERE id=? AND row_revision=? AND state='failed'`), time.Now().UTC(), a.ID, a.RowRevision)
		if updateErr != nil {
			return fmt.Errorf("reclaim comment action: %w", updateErr)
		}
		n, updateErr := result.RowsAffected()
		if updateErr != nil {
			return updateErr
		}
		if n != 1 {
			return ErrMetaCommentActionConflict
		}
		a, readErr = scanMetaAction(tx.QueryRowContext(ctx, s.Rebind(`SELECT `+metaActionColumns+` FROM meta_comment_actions a WHERE a.id=?`), a.ID))
		existing = false
		return readErr
	})
	if err == nil && a.State == "unknown" {
		err = ErrMetaCommentActionUnknown
	}
	return a, existing, err
}

func (s *Store) changeMetaAction(ctx context.Context, id, revision int64, sets, state string, args ...any) (a MetaCommentAction, err error) {
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		args = append(args, time.Now().UTC(), id, revision)
		result, e := tx.ExecContext(ctx, s.Rebind(`UPDATE meta_comment_actions SET `+sets+`,updated_at=?,row_revision=row_revision+1 WHERE id=? AND row_revision=? AND `+state), args...)
		if e != nil {
			return fmt.Errorf("update comment action: %w", e)
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrMetaCommentActionConflict
		}
		a, e = scanMetaAction(tx.QueryRowContext(ctx, s.Rebind(`SELECT `+metaActionColumns+` FROM meta_comment_actions a WHERE a.id=?`), id))
		return e
	})
	return
}
func (s *Store) MarkMetaCommentActionDispatchedContext(ctx context.Context, id, revision int64) (MetaCommentAction, error) {
	return s.changeMetaAction(ctx, id, revision, `state='dispatched',dispatched_at=?`, `state='claimed'`, time.Now().UTC())
}
func (s *Store) FinishMetaCommentActionContext(ctx context.Context, id, revision int64, out MetaCommentActionOutcome) (MetaCommentAction, error) {
	if out.State != "succeeded" && out.State != metaCommentActionFailed && out.State != "unknown" {
		return MetaCommentAction{}, ErrMetaCommentActionConflict
	}
	from := "state='dispatched'"
	if out.State == metaCommentActionFailed {
		from = "state IN ('claimed','dispatched')"
	}
	return s.changeMetaAction(ctx, id, revision, `state=?,error_code=NULLIF(?,''),remote_id=NULLIF(?,''),remote_recipient_id=NULLIF(?,''),reply_message_id=NULLIF(?,0)`, from, out.State, out.ErrorCode, out.RemoteID, out.RemoteRecipientID, out.ReplyMessageID)
}
func (s *Store) ResolveUnknownMetaCommentActionContext(ctx context.Context, id, revision int64, sent bool) (MetaCommentAction, error) {
	state, code := metaCommentActionFailed, "resolved_not_sent"
	if sent {
		state, code = "succeeded", ""
	}
	return s.changeMetaAction(ctx, id, revision, `state=?,error_code=NULLIF(?,'')`, `state='unknown'`, state, code)
}
func (s *Store) ListMetaCommentActionsContext(ctx context.Context, sourceID int64, commentID string) ([]MetaCommentAction, error) {
	return s.listMetaCommentActionsContext(ctx, `a.source_id=? AND a.target_comment_id=?`, sourceID, commentID)
}

// ListMessageMetaCommentActionsContext retains original receipt provenance after
// an archive-only source merge changes a message's current provider identity.
func (s *Store) ListMessageMetaCommentActionsContext(ctx context.Context, messageID int64) ([]MetaCommentAction, error) {
	return s.listMetaCommentActionsContext(ctx, `a.message_id=?`, messageID)
}

// preserveMergedMetaCommentReplies saves observed archive evidence before a
// source merge rewrites provider IDs. The receipt's dispatch identity stays
// unchanged; the local reply reference remains valid across later merges.
func preserveMergedMetaCommentReplies(ctx context.Context, tx *loggedTx, sourceID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta_comment_actions SET reply_message_id=(
		SELECT m.id FROM messages m
		WHERE m.source_id=meta_comment_actions.source_id AND m.source_message_id=meta_comment_actions.remote_id
		LIMIT 1)
		WHERE source_id=? AND action='public_reply' AND reply_message_id IS NULL
		AND EXISTS (SELECT 1 FROM messages m
			WHERE m.source_id=meta_comment_actions.source_id AND m.source_message_id=meta_comment_actions.remote_id)`, sourceID)
	return err
}

func (s *Store) listMetaCommentActionsContext(ctx context.Context, filter string, args ...any) ([]MetaCommentAction, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT `+metaActionColumns+` FROM meta_comment_actions a WHERE `+filter+` ORDER BY a.id`), args...)
	if err != nil {
		return nil, fmt.Errorf("list comment actions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []MetaCommentAction{}
	for rows.Next() {
		a, e := scanMetaAction(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// MetaCommentTarget is a body-free snapshot for binding provider writes.
type MetaCommentTarget struct {
	MessageID, SourceID, ParentMessageID, SenderID    int64
	CommentID, MessageType, Metadata, ParentCommentID string
	SentAt                                            sql.NullTime
	IsFromMe, DeletedFromSource                       bool
}

func (s *Store) MetaCommentTargetContext(ctx context.Context, id int64) (m MetaCommentTarget, err error) {
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT m.id,m.source_id,COALESCE(m.reply_to_message_id,0),COALESCE(m.sender_id,0),m.source_message_id,m.message_type,COALESCE(CAST(m.metadata AS TEXT),''),COALESCE(parent.source_message_id,''),m.sent_at,m.is_from_me,m.deleted_from_source_at IS NOT NULL
 FROM messages m LEFT JOIN messages parent ON parent.id=m.reply_to_message_id AND parent.source_id=m.source_id AND parent.conversation_id=m.conversation_id WHERE m.id=? AND m.deleted_at IS NULL`), id).Scan(&m.MessageID, &m.SourceID, &m.ParentMessageID, &m.SenderID, &m.CommentID, &m.MessageType, &m.Metadata, &m.ParentCommentID, &m.SentAt, &m.IsFromMe, &m.DeletedFromSource)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrMessageNotFound
	}
	return
}
