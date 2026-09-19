package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	IMAPDraftOperationEdit   = "edit"
	IMAPDraftOperationDelete = "delete"
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
	if err := validateIMAPDraftID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	return loadIMAPDraft(ctx, s.db, "", draftID)
}

func validateIMAPDraftID(draftID string) error {
	if strings.TrimSpace(draftID) == "" || strings.ContainsAny(draftID, "\x00\r\n") {
		return errors.New("invalid IMAP draft ID")
	}
	return nil
}

func loadIMAPDraft(
	ctx context.Context,
	q interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	},
	lockClause string,
	draftID string,
) (IMAPDraft, error) {
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

func (s *Store) loadIMAPDraftTx(ctx context.Context, tx *loggedTx, draftID string) (IMAPDraft, error) {
	return loadIMAPDraft(ctx, tx, s.dialect.SelectForUpdate(), draftID)
}

func (s *Store) lockIMAPDraftTx(ctx context.Context, tx *loggedTx, draftID string) error {
	if lockSQL := s.dialect.RowWriterLockSQL("imap_drafts", "updated_at"); lockSQL != "" {
		// Managed drafts use draft_id instead of the dialect helper's id key.
		lockSQL = strings.Replace(lockSQL, "WHERE id = ?", "WHERE draft_id = ?", 1)
		if _, err := tx.ExecContext(ctx, lockSQL, draftID); err != nil {
			return fmt.Errorf("lock IMAP draft %q: %w", draftID, err)
		}
	}
	return nil
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
	if err := validateIMAPDraftID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	if revision <= 0 {
		return IMAPDraft{}, fmt.Errorf("%w: expected positive revision", ErrIMAPDraftRevision)
	}
	if operation != IMAPDraftOperationEdit && operation != IMAPDraftOperationDelete {
		return IMAPDraft{}, fmt.Errorf("%w: unknown operation %q", ErrIMAPDraftState, operation)
	}
	if operation == IMAPDraftOperationEdit && len(replacementRaw) == 0 {
		return IMAPDraft{}, errors.New("edit candidate must not be empty")
	}
	var claimed IMAPDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIMAPDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadIMAPDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return fmt.Errorf("%w: expected %d, found %d", ErrIMAPDraftRevision, revision, draft.Revision)
		}
		if draft.DiscardedAt != nil {
			return fmt.Errorf("%w: draft is discarded", ErrIMAPDraftState)
		}
		if draft.Pending != nil {
			return ErrIMAPDraftPending
		}
		args := []any{
			operation, draft.CurrentMessageID, draft.CurrentReceipt.Mailbox,
			draft.CurrentReceipt.UIDValidity, draft.CurrentReceipt.UID,
		}
		var raw any
		if operation == IMAPDraftOperationEdit {
			raw = append([]byte(nil), replacementRaw...)
		}
		args = append(args, raw)
		result, err := tx.ExecContext(ctx, `
			UPDATE imap_drafts
			SET pending_operation = ?,
			    pending_original_message_id = ?,
			    pending_original_mailbox = ?,
			    pending_original_uidvalidity = ?,
			    pending_original_uid = ?,
			    pending_raw = ?,
			    pending_replacement_mailbox = NULL,
			    pending_replacement_uidvalidity = NULL,
			    pending_replacement_uid = NULL,
			    pending_code = NULL,
			    updated_at = `+s.dialect.Now()+`
			WHERE draft_id = ? AND revision = ? AND pending_operation IS NULL
		`, append(args, draftID, revision)...)
		if err != nil {
			return fmt.Errorf("claim IMAP draft %q: %w", draftID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check IMAP draft claim %q: %w", draftID, err)
		}
		if affected != 1 {
			return ErrIMAPDraftPending
		}
		claimed = draft
		claimed.Pending = &IMAPDraftPending{
			Operation: operation, OriginalMessageID: draft.CurrentMessageID,
			OriginalReceipt: draft.CurrentReceipt, Raw: append([]byte(nil), replacementRaw...),
		}
		if operation == IMAPDraftOperationDelete {
			claimed.Pending.Raw = nil
		}
		return nil
	})
	if err != nil {
		return IMAPDraft{}, err
	}
	return claimed, nil
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
	if err := validateIMAPDraftID(draftID); err != nil {
		return err
	}
	if revision <= 0 || strings.TrimSpace(code) == "" {
		return fmt.Errorf("%w: outcome requires positive revision and code", ErrIMAPDraftState)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIMAPDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadIMAPDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
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
		var result sql.Result
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
	if err := validateIMAPDraftID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	if outcome != "rejected" && outcome != "cancelled" && outcome != "not_attempted" {
		return IMAPDraft{}, errors.New("IMAP draft abort requires a definitive no-effect outcome")
	}
	var active IMAPDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIMAPDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadIMAPDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
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
		if err := s.clearIMAPDraftPendingTx(ctx, tx, draftID, revision); err != nil {
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
	if err := validateIMAPDraftID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	if revision <= 0 || build == nil {
		return IMAPDraft{}, errors.New("invalid IMAP draft publication")
	}
	var published IMAPDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIMAPDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadIMAPDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
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
		var existingMessageID int64
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
		`, draft.SourceID, IMAPDraftSourceMessageID(receipt)).Scan(&existingMessageID); err == nil {
			return fmt.Errorf("replacement source key already belongs to message %d", existingMessageID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check replacement source key: %w", err)
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
			if data.MIMEAttachmentReplacement != nil {
				return nil, errors.New("IMAP draft replacements cannot contain attachments")
			}
			if !bytes.Equal(data.RawMIME, draft.Pending.Raw) {
				return nil, errors.New("replacement MIME does not match the claimed candidate")
			}
			return data, nil
		}
		after := func(ctx context.Context, tx *loggedTx, _ *MessagePersistData, messageID int64) error {
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
			if err := replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, messageID, []int64{labelID}); err != nil {
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
		_, err = s.persistMessageWithParticipantsTx(ctx, tx, nil, participants, build, prepare, after)
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
	if err := validateIMAPDraftID(draftID); err != nil {
		return IMAPDraft{}, err
	}
	var finished IMAPDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIMAPDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadIMAPDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
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
				UPDATE imap_drafts SET discarded_at = %s, revision = revision + 1,
				pending_operation = NULL, pending_original_message_id = NULL,
				pending_original_mailbox = NULL, pending_original_uidvalidity = NULL,
				pending_original_uid = NULL, pending_raw = NULL,
				pending_replacement_mailbox = NULL,
				pending_replacement_uidvalidity = NULL, pending_replacement_uid = NULL,
				pending_code = NULL, updated_at = %s
				WHERE draft_id = ? AND revision = ? AND pending_operation = 'delete'
			`, s.dialect.Now(), s.dialect.Now()), draftID, revision); err != nil {
				return fmt.Errorf("discard IMAP draft %q: %w", draftID, err)
			}
			finished = draft
			finished.Revision++
			finished.DiscardedAt = ptrTimeNow()
			finished.Pending = nil
			return nil
		}
		if err := s.clearIMAPDraftPendingTx(ctx, tx, draftID, revision); err != nil {
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

func (s *Store) clearIMAPDraftPendingTx(ctx context.Context, tx *loggedTx, draftID string, revision int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE imap_drafts
		SET pending_operation = NULL, pending_original_message_id = NULL,
		    pending_original_mailbox = NULL, pending_original_uidvalidity = NULL,
		    pending_original_uid = NULL, pending_raw = NULL,
		    pending_replacement_mailbox = NULL,
		    pending_replacement_uidvalidity = NULL, pending_replacement_uid = NULL,
		    pending_code = NULL, updated_at = `+s.dialect.Now()+`
		WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL
	`, draftID, revision)
	if err != nil {
		return fmt.Errorf("clear IMAP draft pending evidence %q: %w", draftID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrIMAPDraftState
	}
	return nil
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
	if err := replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, messageID, labelIDs); err != nil {
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
