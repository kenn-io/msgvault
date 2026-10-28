package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	IMAPDraftOperationEdit   = draftOperationEdit
	IMAPDraftOperationDelete = draftOperationDelete
	IMAPDraftCodeRejected    = "append_rejected"
	IMAPDraftCodeCleanup     = "cleanup_pending"
	IMAPDraftCodeRemoved     = "removed"
)

// GetMessageReplyToMessageIDContext reads the archived reply link needed when
// an owned draft is replaced. It is a direct message-row lookup.
func (s *Store) GetMessageReplyToMessageIDContext(ctx context.Context, messageID int64) (sql.NullInt64, error) {
	var replyTo sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		s.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), messageID,
	).Scan(&replyTo)
	return replyTo, err
}

// GetIMAPDraft returns one managed draft and its durable pending evidence.
func (s *Store) GetIMAPDraft(draftID string) (IMAPDraft, error) {
	return s.GetIMAPDraftContext(context.Background(), draftID)
}

// GetIMAPDraftContext reads ownership by draft ID. It never consults live
// mailbox memberships, so a sync observation cannot change mutation authority.
func (s *Store) GetIMAPDraftContext(ctx context.Context, draftID string) (IMAPDraft, error) {
	if err := imapDrafts.validateID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	return loadIMAPDraft(ctx, s.db, "", draftID)
}

var imapDrafts = draftTable[IMAPDraft]{
	provider:           "IMAP",
	table:              "imap_drafts",
	originalColumns:    []string{"pending_original_mailbox", "pending_original_uidvalidity", "pending_original_uid"},
	replacementColumns: []string{"pending_replacement_mailbox", "pending_replacement_uidvalidity", "pending_replacement_uid"},
	errRevision:        ErrIMAPDraftRevision,
	errPending:         ErrIMAPDraftPending,
	errState:           ErrIMAPDraftState,
	load:               loadIMAPDraft,
	state: func(d IMAPDraft) draftState {
		return draftState{revision: d.Revision, discarded: d.DiscardedAt != nil, pending: d.Pending != nil}
	},
	originalArgs: func(d IMAPDraft) []any {
		return []any{d.CurrentMessageID, d.CurrentReceipt.Mailbox, d.CurrentReceipt.UIDValidity, d.CurrentReceipt.UID}
	},
	withClaim: func(d IMAPDraft, operation string, raw []byte) IMAPDraft {
		d.Pending = &IMAPDraftPending{
			Operation: operation, OriginalMessageID: d.CurrentMessageID,
			OriginalReceipt: d.CurrentReceipt, Raw: raw,
		}
		return d
	},
}

func loadIMAPDraft(ctx context.Context, q contextRowQuerier, lockClause string, draftID string) (IMAPDraft, error) {
	var (
		draft                                  IMAPDraft
		discardedAt                            nullableTimestamp
		pendingOperation                       sql.NullString
		pendingOriginalMessageID               sql.NullInt64
		pendingOriginalMailbox                 sql.NullString
		pendingOriginalUIDValidity, pendingUID sql.NullInt64
		pendingRaw                             []byte
		pendingReplacementMailbox              sql.NullString
		pendingReplacementUIDValidity          sql.NullInt64
		pendingReplacementUID                  sql.NullInt64
		pendingCode                            sql.NullString
	)
	err := q.QueryRowContext(ctx, `
		SELECT draft_id, source_id, current_message_id, current_mailbox,
		       current_uidvalidity, current_uid, revision, discarded_at,
		       pending_operation, pending_original_message_id,
		       pending_original_mailbox, pending_original_uidvalidity,
		       pending_original_uid, pending_raw, pending_replacement_mailbox,
		       pending_replacement_uidvalidity, pending_replacement_uid,
		       pending_code
		FROM imap_drafts
		WHERE draft_id = ?`+lockClause, draftID).Scan(
		&draft.DraftID, &draft.SourceID, &draft.CurrentMessageID,
		&draft.CurrentReceipt.Mailbox, &draft.CurrentReceipt.UIDValidity,
		&draft.CurrentReceipt.UID, &draft.Revision, &discardedAt,
		&pendingOperation, &pendingOriginalMessageID, &pendingOriginalMailbox,
		&pendingOriginalUIDValidity, &pendingUID, &pendingRaw,
		&pendingReplacementMailbox, &pendingReplacementUIDValidity,
		&pendingReplacementUID, &pendingCode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return IMAPDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrIMAPDraftNotFound)
	}
	if err != nil {
		return IMAPDraft{}, fmt.Errorf("load IMAP draft %q: %w", draftID, err)
	}
	draft.CurrentReceipt.SourceID = draft.SourceID
	if discardedAt.Valid {
		t := discardedAt.Time
		draft.DiscardedAt = &t
	}
	if pendingOperation.Valid {
		if !pendingOriginalMessageID.Valid || !pendingOriginalMailbox.Valid ||
			!pendingOriginalUIDValidity.Valid || !pendingUID.Valid {
			return IMAPDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrIMAPDraftState)
		}
		originalUIDValidity, err := checkedIMAPDraftUint32(pendingOriginalUIDValidity.Int64)
		if err != nil {
			return IMAPDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrIMAPDraftState)
		}
		originalUID, err := checkedIMAPDraftUint32(pendingUID.Int64)
		if err != nil {
			return IMAPDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrIMAPDraftState)
		}
		pending := &IMAPDraftPending{
			Operation:         pendingOperation.String,
			OriginalMessageID: pendingOriginalMessageID.Int64,
			OriginalReceipt: IMAPDraftReceipt{
				SourceID: draft.SourceID, Mailbox: pendingOriginalMailbox.String,
				UIDValidity: originalUIDValidity, UID: originalUID,
			},
			Raw:  append([]byte(nil), pendingRaw...),
			Code: pendingCode.String,
		}
		if pendingReplacementMailbox.Valid {
			if !pendingReplacementUIDValidity.Valid || !pendingReplacementUID.Valid {
				return IMAPDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrIMAPDraftState)
			}
			replacementUIDValidity, err := checkedIMAPDraftUint32(pendingReplacementUIDValidity.Int64)
			if err != nil {
				return IMAPDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrIMAPDraftState)
			}
			replacementUID, err := checkedIMAPDraftUint32(pendingReplacementUID.Int64)
			if err != nil {
				return IMAPDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrIMAPDraftState)
			}
			pending.ReplacementReceipt = &IMAPDraftReceipt{
				SourceID: draft.SourceID, Mailbox: pendingReplacementMailbox.String,
				UIDValidity: replacementUIDValidity, UID: replacementUID,
			}
		}
		draft.Pending = pending
	}
	return draft, nil
}

func checkedIMAPDraftUint32(value int64) (uint32, error) {
	if value <= 0 || value > int64(^uint32(0)) {
		return 0, errors.New("draft receipt value is outside uint32 range")
	}
	return uint32(value), nil
}

// ClaimIMAPDraftContext durably records the original receipt and candidate
// bytes before a provider mutation. The revision remains unchanged.
func (s *Store) ClaimIMAPDraftContext(
	ctx context.Context,
	draftID string,
	revision int64,
	operation string,
	replacementRaw []byte,
) (IMAPDraft, error) {
	return imapDrafts.claim(ctx, s, draftID, revision, operation, replacementRaw)
}

// RecordIMAPDraftOutcomeContext records a provider result against the current
// pending attempt. It never publishes a replacement or clears the claim.
func (s *Store) RecordIMAPDraftOutcomeContext(
	ctx context.Context,
	draftID string,
	revision int64,
	code string,
	replacement *IMAPDraftReceipt,
) error {
	if err := imapDrafts.validateID(draftID); err != nil {
		return err
	}
	if err := imapDrafts.checkOutcomeRequest(revision, code); err != nil {
		return err
	}
	return imapDrafts.inTx(ctx, s, draftID, func(tx *loggedTx, draft IMAPDraft) error {
		if draft.Revision != revision {
			return fmt.Errorf("%w: expected %d, found %d", ErrIMAPDraftRevision, revision, draft.Revision)
		}
		if draft.Pending == nil {
			return ErrIMAPDraftState
		}
		if replacement != nil {
			if replacement.SourceID == 0 {
				replacementCopy := *replacement
				replacementCopy.SourceID = draft.SourceID
				replacement = &replacementCopy
			}
			if replacement.SourceID != draft.SourceID {
				return errors.New("replacement receipt source does not match draft")
			}
			if err := validateIMAPDraftReceipt(*replacement); err != nil {
				return err
			}
			if draft.Pending.Operation != IMAPDraftOperationEdit {
				return errors.New("delete outcome cannot carry a replacement receipt")
			}
		}
		var (
			result sql.Result
			err    error
		)
		if replacement == nil {
			result, err = tx.ExecContext(ctx, `
				UPDATE imap_drafts
				SET pending_code = ?, updated_at = `+s.dialect.Now()+`
				WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL
			`, code, draftID, revision)
		} else {
			result, err = tx.ExecContext(ctx, `
				UPDATE imap_drafts
				SET pending_code = ?, pending_replacement_mailbox = ?,
				    pending_replacement_uidvalidity = ?, pending_replacement_uid = ?,
				    updated_at = `+s.dialect.Now()+`
				WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL
			`, code, replacement.Mailbox, replacement.UIDValidity, replacement.UID, draftID, revision)
		}
		if err != nil {
			return fmt.Errorf("record IMAP draft outcome %q: %w", draftID, err)
		}
		if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
			return affectedErr
		} else if affected != 1 {
			return ErrIMAPDraftState
		}
		return nil
	})
}

// AbortIMAPDraftContext clears an unadvanced operation after a proven no-effect
// APPEND or a deletion for which no provider write was attempted.
func (s *Store) AbortIMAPDraftContext(ctx context.Context, draftID string, revision int64, outcome string) (IMAPDraft, error) {
	if err := imapDrafts.validateID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	if outcome != "rejected" && outcome != "cancelled" && outcome != "not_attempted" {
		return IMAPDraft{}, errors.New("IMAP draft abort requires a definitive no-effect outcome")
	}
	var active IMAPDraft
	err := imapDrafts.inTx(ctx, s, draftID, func(tx *loggedTx, draft IMAPDraft) error {
		if draft.Revision != revision {
			return ErrIMAPDraftRevision
		}
		if draft.DiscardedAt != nil || draft.Pending == nil ||
			draft.Pending.ReplacementReceipt != nil || draft.CurrentMessageID != draft.Pending.OriginalMessageID ||
			draft.CurrentReceipt != draft.Pending.OriginalReceipt {
			return errors.New("IMAP draft abort requires an unadvanced operation without an accepted replacement")
		}
		if (draft.Pending.Operation == IMAPDraftOperationDelete) != (outcome == "not_attempted") {
			return errors.New("IMAP draft abort outcome does not match the pending operation")
		}
		if err := imapDrafts.clearPendingTx(ctx, s, tx, draftID, revision); err != nil {
			return err
		}
		active = draft
		active.Pending = nil
		return nil
	})
	if err != nil {
		return IMAPDraft{}, err
	}
	return active, nil
}

// PublishIMAPDraftReplacementContext stores the replacement message and
// advances the public revision while retaining the original cleanup evidence.
func (s *Store) PublishIMAPDraftReplacementContext(
	ctx context.Context,
	draftID string,
	revision int64,
	participants []ParticipantPersistData,
	build func([]int64) *MessagePersistData,
) (IMAPDraft, error) {
	if err := imapDrafts.validateID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	if revision <= 0 || build == nil {
		return IMAPDraft{}, errors.New("invalid IMAP draft publication")
	}
	var published IMAPDraft
	err := imapDrafts.inAttributionTx(ctx, s, draftID, func(tx *loggedTx, draft IMAPDraft) error {
		if draft.Revision != revision {
			return ErrIMAPDraftRevision
		}
		if draft.Pending == nil || draft.Pending.Operation != IMAPDraftOperationEdit || draft.Pending.ReplacementReceipt == nil {
			return errors.New("IMAP draft replacement receipt is not recorded")
		}
		receipt := *draft.Pending.ReplacementReceipt
		if err := invalidatePreviousIMAPDraftSourceKey(ctx, tx, receipt); err != nil {
			return err
		}
		if err := checkDraftReplacementSourceKeyTx(ctx, tx, draft.SourceID, IMAPDraftSourceMessageID(receipt)); err != nil {
			return err
		}
		var existingMembershipID int64
		if err := tx.QueryRowContext(ctx, `
			SELECT message_id FROM imap_message_memberships
			WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
		`, receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID).Scan(&existingMembershipID); err == nil {
			return fmt.Errorf("replacement receipt already belongs to message %d", existingMembershipID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check replacement receipt: %w", err)
		}
		prepare := func(_ context.Context, _ *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
			if data == nil || data.Message == nil {
				return nil, errors.New("persist IMAP draft replacement requires a message")
			}
			if data.Message.SourceID != draft.SourceID || data.Message.SourceMessageID != IMAPDraftSourceMessageID(receipt) {
				return nil, errors.New("replacement message identity does not match receipt")
			}
			if !bytes.Equal(data.RawMIME, draft.Pending.Raw) {
				return nil, errors.New("replacement MIME does not match the claimed candidate")
			}
			return data, nil
		}
		after := func(ctx context.Context, tx *loggedTx, data *MessagePersistData, messageID int64) error {
			q := boundQuerier{ctx: ctx, q: tx}
			if err := s.replaceMIMEAttachmentsWith(q, messageID, data.MIMEAttachmentReplacement); err != nil {
				return fmt.Errorf("persist replacement IMAP attachments: %w", err)
			}
			if err := recomputeMessageAttachmentStatsWith(q, messageID); err != nil {
				return fmt.Errorf("recompute replacement IMAP attachment stats: %w", err)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
				INSERT INTO imap_message_memberships
					(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
				VALUES (?, ?, ?, ?, ?, %s, %s)
			`, s.dialect.JSONBindExpr(), s.dialect.Now()), receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID, messageID, imapDraftFlagsJSON); err != nil {
				return fmt.Errorf("persist replacement IMAP membership: %w", err)
			}
			labelID, err := ensureIMAPMailboxLabel(ctx, tx, receipt.SourceID, receipt.Mailbox)
			if err != nil {
				return err
			}
			if err := s.refreshAccountAttributionIfOutboundChangedTx(ctx, tx, messageID, func() error {
				return replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, messageID, []int64{labelID})
			}); err != nil {
				return fmt.Errorf("persist replacement IMAP label: %w", err)
			}
			result, err := tx.ExecContext(ctx, fmt.Sprintf(`
				UPDATE imap_drafts
				SET current_message_id = ?, current_mailbox = ?,
				    current_uidvalidity = ?, current_uid = ?,
				    revision = revision + 1, pending_code = ?, updated_at = %s
				WHERE draft_id = ? AND revision = ? AND pending_operation = 'edit'
			`, s.dialect.Now()), messageID, receipt.Mailbox, receipt.UIDValidity, receipt.UID,
				IMAPDraftCodeCleanup, draftID, revision)
			if err != nil {
				return fmt.Errorf("publish IMAP draft replacement %q: %w", draftID, err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return ErrIMAPDraftRevision
			}
			published = draft
			published.CurrentMessageID = messageID
			published.CurrentReceipt = receipt
			published.Revision++
			published.Pending = &IMAPDraftPending{
				Operation: draft.Pending.Operation, OriginalMessageID: draft.Pending.OriginalMessageID,
				OriginalReceipt: draft.Pending.OriginalReceipt, Raw: append([]byte(nil), draft.Pending.Raw...),
				ReplacementReceipt: &receipt, Code: IMAPDraftCodeCleanup,
			}
			return nil
		}
		_, err := s.persistMessageWithParticipantsTx(ctx, tx, nil, participants, build, prepare, after)
		return err
	})
	if err != nil {
		return IMAPDraft{}, err
	}
	return published, nil
}

// FinishIMAPDraftRemovalContext records confirmed exact absence. For an edit
// it clears the old cleanup evidence without another revision advance. For a
// delete it marks the current message discarded and advances the revision.
func (s *Store) FinishIMAPDraftRemovalContext(ctx context.Context, draftID string, revision int64) (IMAPDraft, error) {
	if err := imapDrafts.validateID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	var finished IMAPDraft
	err := imapDrafts.inAttributionTx(ctx, s, draftID, func(tx *loggedTx, draft IMAPDraft) error {
		if draft.Revision != revision {
			return ErrIMAPDraftRevision
		}
		if draft.Pending == nil {
			return ErrIMAPDraftState
		}
		if draft.Pending.Code != IMAPDraftCodeRemoved {
			return fmt.Errorf("%w: pending removal code is %q, want %q", ErrIMAPDraftState, draft.Pending.Code, IMAPDraftCodeRemoved)
		}
		if draft.Pending.Operation == IMAPDraftOperationEdit && draft.Pending.ReplacementReceipt == nil {
			return errors.New("cannot finish edit before replacement publication")
		}
		if err := s.retireIMAPDraftMembershipTx(ctx, tx, draft.Pending.OriginalMessageID, draft.Pending.OriginalReceipt); err != nil {
			return err
		}
		if draft.Pending.Operation == IMAPDraftOperationDelete {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
				UPDATE imap_drafts SET discarded_at = %s, revision = revision + 1, %s, updated_at = %s
				WHERE draft_id = ? AND revision = ? AND pending_operation = 'delete'
			`, s.dialect.Now(), imapDrafts.pendingNullSQL(), s.dialect.Now()), draftID, revision); err != nil {
				return fmt.Errorf("discard IMAP draft %q: %w", draftID, err)
			}
			finished = draft
			finished.Revision++
			finished.DiscardedAt = ptrTimeNow()
			finished.Pending = nil
			return nil
		}
		if err := imapDrafts.clearPendingTx(ctx, s, tx, draftID, revision); err != nil {
			return err
		}
		finished = draft
		finished.Pending = nil
		return nil
	})
	if err != nil {
		return IMAPDraft{}, err
	}
	return finished, nil
}

func ptrTimeNow() *time.Time {
	t := time.Now()
	return &t
}

func (s *Store) retireIMAPDraftMembershipTx(
	ctx context.Context,
	tx *loggedTx,
	messageID int64,
	receipt IMAPDraftReceipt,
) error {
	if messageID <= 0 || receipt.SourceID <= 0 {
		return errors.New("invalid IMAP draft cleanup identity")
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ? AND message_id = ?
	`, receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID, messageID); err != nil {
		return fmt.Errorf("retire IMAP draft membership: %w", err)
	}
	mailboxes, err := imapMembershipMailboxes(ctx, tx, receipt.SourceID, messageID)
	if err != nil {
		return err
	}
	labelIDs := make([]int64, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		labelID, err := ensureIMAPMailboxLabel(ctx, tx, receipt.SourceID, mailbox)
		if err != nil {
			return err
		}
		labelIDs = append(labelIDs, labelID)
	}
	if err := s.refreshAccountAttributionIfOutboundChangedTx(ctx, tx, messageID, func() error {
		return replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, messageID, labelIDs)
	}); err != nil {
		return fmt.Errorf("rebuild IMAP draft labels: %w", err)
	}
	if len(mailboxes) == 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP
			WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL
		`, messageID, receipt.SourceID); err != nil {
			return fmt.Errorf("tombstone retired IMAP draft: %w", err)
		}
	}
	if err := s.bumpDerivedDataRevision(tx); err != nil {
		return fmt.Errorf("bump derived-data revision for retired IMAP draft: %w", err)
	}
	return nil
}
