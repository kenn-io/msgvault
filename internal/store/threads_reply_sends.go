package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrThreadsReplyNotFound = errors.New("Threads reply receipt not found")
	ErrThreadsReplyConflict = errors.New("Threads reply receipt conflict")
	ErrChatDraftClaimed     = errors.New("chat draft is claimed by a reply attempt")
)

type ChatDraftClaimedError struct{ DraftID string }

func (e *ChatDraftClaimedError) Error() string        { return ErrChatDraftClaimed.Error() }
func (e *ChatDraftClaimedError) Is(target error) bool { return target == ErrChatDraftClaimed }

type ThreadsReplySend struct {
	DraftID                                                           string
	SourceID                                                          int64
	SourceIdentifier                                                  string
	ConversationID                                                    int64
	DraftRevision                                                     int64
	RootMediaID, ParentMediaID, Body, BodyHash                        string
	RowRevision                                                       int64
	State, ContainerID, MediaID                                       string
	CreatedAt, UpdatedAt                                              time.Time
	ContainerCreatedAt, LastStatusAt, NextPollAt, PublishDispatchedAt *time.Time
	ArchiveComplete                                                   bool
	LocalMessageID                                                    int64
}
type ThreadsReplyTransition struct {
	State, ContainerID, MediaID                  string
	ContainerCreatedAt, LastStatusAt, NextPollAt *time.Time
}

const threadsReplyColumns = `draft_id,source_id,source_identifier,conversation_id,draft_revision,
 root_media_id,parent_media_id,body,body_hash,row_revision,state,container_id,media_id,
 created_at,updated_at,container_created_at,last_status_at,next_poll_at,publish_dispatched_at,
 archive_complete,local_message_id`

func scanThreadsReply(row scanner) (ThreadsReplySend, error) {
	var r ThreadsReplySend
	var container, media sql.NullString
	var created, updated requiredTimestamp
	var containerAt, statusAt, pollAt, dispatchAt nullableTimestamp
	var local sql.NullInt64
	err := row.Scan(&r.DraftID, &r.SourceID, &r.SourceIdentifier, &r.ConversationID, &r.DraftRevision,
		&r.RootMediaID, &r.ParentMediaID, &r.Body, &r.BodyHash, &r.RowRevision, &r.State, &container, &media,
		&created, &updated, &containerAt, &statusAt, &pollAt, &dispatchAt, &r.ArchiveComplete, &local)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrThreadsReplyNotFound
	}
	if err != nil {
		return r, fmt.Errorf("read Threads reply receipt: %w", err)
	}
	r.ContainerID, r.MediaID, r.LocalMessageID = container.String, media.String, local.Int64
	r.CreatedAt, r.UpdatedAt = created.Time.UTC(), updated.Time.UTC()
	r.ContainerCreatedAt, r.LastStatusAt, r.NextPollAt, r.PublishDispatchedAt = optionalTimestamp(containerAt), optionalTimestamp(statusAt), optionalTimestamp(pollAt), optionalTimestamp(dispatchAt)
	return r, nil
}
func (s *Store) GetThreadsReplySendContext(ctx context.Context, id string) (ThreadsReplySend, error) {
	return scanThreadsReply(s.db.QueryRowContext(ctx, `SELECT `+threadsReplyColumns+` FROM threads_reply_sends WHERE draft_id=?`, id))
}
func (s *Store) loadThreadsReplyTx(ctx context.Context, tx *loggedTx, id string) (ThreadsReplySend, error) {
	return scanThreadsReply(tx.QueryRowContext(ctx, `SELECT `+threadsReplyColumns+` FROM threads_reply_sends WHERE draft_id=?`+s.dialect.SelectForUpdate(), id))
}

// Every draft mutation takes this lock before consulting its receipt. SQLite
// acquires a writer reservation before any reads; PostgreSQL locks the same row.
// An absent draft is normal for a surviving completed receipt.
func (s *Store) lockChatDraftTx(ctx context.Context, tx *loggedTx, id string) error {
	if !s.IsPostgreSQL() {
		_, err := tx.ExecContext(ctx, `UPDATE chat_drafts SET revision=revision WHERE draft_id=?`, id)
		return err
	}
	var locked string
	err := tx.QueryRowContext(ctx, `SELECT draft_id FROM chat_drafts WHERE draft_id=? FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
func threadsUnpublishedTerminal(r ThreadsReplySend) bool {
	if r.PublishDispatchedAt != nil || r.MediaID != "" {
		return false
	}
	switch r.State {
	case "rate_limited", "reply_not_permitted", "invalid_body", "send_failed":
		return true
	}
	return false
}
func (s *Store) guardChatDraftMutationTx(ctx context.Context, tx *loggedTx, id string) error {
	if err := s.lockChatDraftTx(ctx, tx, id); err != nil {
		return err
	}
	// Missing drafts keep the ordinary not-found contract, without disclosing a
	// receipt through the draft lifecycle path.
	if _, err := loadChatDraft(ctx, tx, id); err != nil {
		return err
	}
	r, err := s.loadThreadsReplyTx(ctx, tx, id)
	if errors.Is(err, ErrThreadsReplyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !threadsUnpublishedTerminal(r) {
		return &ChatDraftClaimedError{DraftID: id}
	}
	return nil
}
func threadsReplyDecimalID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return strings.Trim(id, "0") != ""
}
func validateThreadsReplySourceTx(ctx context.Context, tx *loggedTx, r ThreadsReplySend) error {
	source, err := scanSource(tx.QueryRowContext(ctx, `SELECT `+sourceCatalogColumns+` FROM sources WHERE id=?`, r.SourceID))
	if err != nil {
		return fmt.Errorf("Threads receipt source unavailable: %w", err)
	}
	if source.SourceType != "threads" || source.Identifier != r.SourceIdentifier || source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return ErrSourceRetired
	}
	return nil
}
func (s *Store) ClaimThreadsReplySendContext(ctx context.Context, id string, revision int64) (ThreadsReplySend, error) {
	var receipt ThreadsReplySend
	if revision <= 0 {
		return receipt, ErrChatDraftRevisionConflict
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockChatDraftTx(ctx, tx, id); err != nil {
			return err
		}
		old, err := s.loadThreadsReplyTx(ctx, tx, id)
		if err == nil {
			if old.DraftRevision != revision {
				return ErrChatDraftRevisionConflict
			}
			if err = validateThreadsReplySourceTx(ctx, tx, old); err != nil {
				return err
			}
			receipt = old
			return nil
		}
		if !errors.Is(err, ErrThreadsReplyNotFound) {
			return err
		}
		draft, err := loadChatDraft(ctx, tx, id)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrChatDraftRevisionConflict
		}
		if draft.SourceType != "threads" || !threadsReplyDecimalID(draft.SourceConversationID) || !threadsReplyDecimalID(draft.ReplyToSourceMessageID) {
			return ErrChatDraftInvalidDestination
		}
		receipt = ThreadsReplySend{DraftID: id, SourceID: draft.SourceID, SourceIdentifier: draft.SourceIdentifier, ConversationID: draft.ConversationID, DraftRevision: revision, RootMediaID: draft.SourceConversationID, ParentMediaID: draft.ReplyToSourceMessageID, Body: draft.Body}
		if err = validateThreadsReplySourceTx(ctx, tx, receipt); err != nil {
			return err
		}
		if err = threadsReplyTargetContext(ctx, tx, receipt.SourceID, receipt.ConversationID, receipt.RootMediaID, receipt.ParentMediaID); err != nil {
			return err
		}

		receipt.BodyHash = fmt.Sprintf("%x", sha256.Sum256([]byte(receipt.Body)))
		_, err = tx.ExecContext(ctx, `INSERT INTO threads_reply_sends(draft_id,source_id,source_identifier,conversation_id,draft_revision,root_media_id,parent_media_id,body,body_hash,state)
   VALUES(?,?,?,?,?,?,?,?,?,'claimed')`, id, receipt.SourceID, receipt.SourceIdentifier, receipt.ConversationID, revision, receipt.RootMediaID, receipt.ParentMediaID, receipt.Body, receipt.BodyHash)
		if err != nil {
			return fmt.Errorf("claim Threads draft: %w", err)
		}
		receipt, err = s.loadThreadsReplyTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return ThreadsReplySend{}, err
	}
	return receipt, nil
}

func validThreadsTransition(r ThreadsReplySend, t ThreadsReplyTransition) bool {
	// A known container and its processing clock are one durable transition.
	if r.ContainerID == "" && (t.ContainerID != "" || t.ContainerCreatedAt != nil || t.LastStatusAt != nil || t.NextPollAt != nil) {
		if r.State != "claimed" || t.State != "container_saved" || t.ContainerID == "" || t.ContainerCreatedAt == nil {
			return false
		}
	}
	if r.State == "complete" || threadsUnpublishedTerminal(r) {
		return false
	}
	state := t.State
	if state == "" {
		state = r.State
	}
	if r.PublishDispatchedAt != nil {
		return (r.State == "publish_dispatched" || r.State == "remote_unknown" || r.State == "sent_archive_pending") &&
			((state == "remote_unknown" && r.MediaID == "" && t.MediaID == "") || (state == "sent_archive_pending" && (r.MediaID != "" || t.MediaID != "")))
	}
	if state == "publish_dispatched" || state == "remote_unknown" || state == "sent_archive_pending" || state == "complete" || t.MediaID != "" {
		return false
	}
	if state == "rate_limited" || state == "reply_not_permitted" || state == "invalid_body" || state == "send_failed" {
		return true
	}
	switch r.State {
	case "claimed":
		return state == "claimed" || (state == "container_saved" && t.ContainerID != "" && t.ContainerCreatedAt != nil)
	case "container_saved":
		return state == "container_saved" || state == "ready"
	case "ready":
		return state == "ready"
	}
	return false
}
func (s *Store) AdvanceThreadsReplySendContext(ctx context.Context, id string, revision int64, t ThreadsReplyTransition) (ThreadsReplySend, error) {
	var result ThreadsReplySend
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockChatDraftTx(ctx, tx, id); err != nil {
			return err
		}
		r, err := s.loadThreadsReplyTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.RowRevision != revision {
			return ErrThreadsReplyConflict
		}
		if err = validateThreadsReplySourceTx(ctx, tx, r); err != nil {
			return err
		}
		if !validThreadsTransition(r, t) {
			return fmt.Errorf("invalid Threads receipt transition: %w", ErrThreadsReplyConflict)
		}
		if t.ContainerID != "" {
			if !threadsReplyDecimalID(t.ContainerID) || (r.ContainerID != "" && r.ContainerID != t.ContainerID) || r.PublishDispatchedAt != nil {
				return ErrThreadsReplyConflict
			}
			r.ContainerID = t.ContainerID
		}
		if t.MediaID != "" {
			if !threadsReplyDecimalID(t.MediaID) || (r.MediaID != "" && r.MediaID != t.MediaID) {
				return ErrThreadsReplyConflict
			}
			r.MediaID = t.MediaID
		}
		if t.ContainerCreatedAt != nil {
			if t.ContainerCreatedAt.IsZero() || (r.ContainerCreatedAt != nil && !r.ContainerCreatedAt.Equal(*t.ContainerCreatedAt)) {
				return ErrThreadsReplyConflict
			}
			r.ContainerCreatedAt = t.ContainerCreatedAt
		}
		for _, pair := range [][2]*time.Time{{r.LastStatusAt, t.LastStatusAt}, {r.NextPollAt, t.NextPollAt}} {
			if pair[1] != nil && (pair[1].IsZero() || (pair[0] != nil && pair[1].Before(*pair[0]))) {
				return ErrThreadsReplyConflict
			}
		}
		if t.LastStatusAt != nil {
			r.LastStatusAt = t.LastStatusAt
		}
		if t.NextPollAt != nil {
			r.NextPollAt = t.NextPollAt
		}
		if t.State != "" {
			r.State = t.State
		}
		if (r.State == "container_saved" || r.State == "ready") && (r.ContainerID == "" || r.ContainerCreatedAt == nil) {
			return ErrThreadsReplyConflict
		}
		res, err := tx.ExecContext(ctx, `UPDATE threads_reply_sends SET state=?,container_id=?,media_id=?,container_created_at=?,last_status_at=?,next_poll_at=?,row_revision=row_revision+1,updated_at=`+s.dialect.Now()+` WHERE draft_id=? AND row_revision=?`, r.State, nullThreadsID(r.ContainerID), nullThreadsID(r.MediaID), r.ContainerCreatedAt, r.LastStatusAt, r.NextPollAt, id, revision)
		if err = threadsReplyCAS(res, err); err != nil {
			return err
		}
		result, err = s.loadThreadsReplyTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return ThreadsReplySend{}, err
	}
	return result, nil
}
func nullThreadsID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
func threadsReplyCAS(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrThreadsReplyConflict
	}
	return nil
}

// Only a confirmed commit winner may dispatch. A commit error deliberately
// loses authorization even if the database might have accepted the barrier.
func (s *Store) TryDispatchThreadsReplyContext(ctx context.Context, id string, revision int64) (bool, error) {
	candidate := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockChatDraftTx(ctx, tx, id); err != nil {
			return err
		}
		r, err := s.loadThreadsReplyTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = validateThreadsReplySourceTx(ctx, tx, r); err != nil {
			return err
		}
		if r.RowRevision != revision || r.State != "ready" || r.PublishDispatchedAt != nil || r.ContainerID == "" || r.MediaID != "" {
			return ErrThreadsReplyConflict
		}
		res, err := tx.ExecContext(ctx, `UPDATE threads_reply_sends SET state='publish_dispatched',publish_dispatched_at=`+s.dialect.Now()+`,row_revision=row_revision+1,updated_at=`+s.dialect.Now()+` WHERE draft_id=? AND row_revision=? AND state='ready' AND publish_dispatched_at IS NULL`, id, revision)
		if err = threadsReplyCAS(res, err); err != nil {
			return err
		}
		candidate = true
		return nil
	})
	return candidate && err == nil, err
}
func (s *Store) FinishThreadsReplyArchiveContext(ctx context.Context, id string, revision, messageID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockChatDraftTx(ctx, tx, id); err != nil {
			return err
		}
		r, err := s.loadThreadsReplyTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.DraftRevision != revision {
			return ErrChatDraftRevisionConflict
		}
		if err = validateThreadsReplySourceTx(ctx, tx, r); err != nil {
			return err
		}
		if r.MediaID == "" || r.PublishDispatchedAt == nil || (r.State != "sent_archive_pending" && r.State != "complete") {
			return ErrThreadsReplyConflict
		}
		var valid bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=? AND source_id=? AND source_message_id=? AND conversation_id=? AND message_type='threads')`, messageID, r.SourceID, r.MediaID, r.ConversationID).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrChatDraftInvalidDestination
		}
		if r.State == "complete" {
			if r.LocalMessageID != messageID {
				return ErrThreadsReplyConflict
			}
			return nil
		}
		draft, err := loadChatDraft(ctx, tx, id)
		if err != nil && !errors.Is(err, ErrChatDraftNotFound) {
			return err
		}
		if err == nil {
			if draft.Revision != revision {
				return ErrChatDraftRevisionConflict
			}
			res, err := tx.ExecContext(ctx, `DELETE FROM chat_drafts WHERE draft_id=? AND revision=?`, id, revision)
			if err = threadsReplyCAS(res, err); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE threads_reply_sends SET state='complete',archive_complete=TRUE,local_message_id=?,row_revision=row_revision+1,updated_at=`+s.dialect.Now()+` WHERE draft_id=? AND row_revision=?`, messageID, id, r.RowRevision)
		return threadsReplyCAS(res, err)
	})
}

// ValidateThreadsReplyTargetContext checks the live archived destination before
// a sender opens credentials. Claim repeats it under the draft transaction.
func (s *Store) ValidateThreadsReplyTargetContext(ctx context.Context, sourceID, conversationID int64, root, parent string) error {
	return threadsReplyTargetContext(ctx, s.db, sourceID, conversationID, root, parent)
}
func threadsReplyTargetContext(ctx context.Context, q contextRowQuerier, sourceID, conversationID int64, root, parent string) error {
	var found bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN conversations c ON c.id=m.conversation_id
 WHERE m.source_id=? AND c.source_id=m.source_id AND c.id=? AND c.source_conversation_id=? AND m.source_message_id=? AND m.message_type='threads' AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)`, sourceID, conversationID, root, parent).Scan(&found)
	if err != nil {
		return err
	}
	if !found {
		return ErrChatDraftInvalidDestination
	}
	return nil
}
