package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const imapDraftFlagsJSON = `["\\Draft"]`

// IMAPDraftReceipt identifies the provider message created by APPEND.
type IMAPDraftReceipt struct {
	SourceID    int64
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// IMAPDraftPending contains durable evidence for an edit or delete that has
// not completed its provider cleanup.
type IMAPDraftPending struct {
	Operation          string
	OriginalMessageID  int64
	OriginalReceipt    IMAPDraftReceipt
	Raw                []byte
	ReplacementReceipt *IMAPDraftReceipt
	Code               string
}

// IMAPDraft is the Store-owned lifecycle record for one managed draft.
type IMAPDraft struct {
	DraftID          string
	SourceID         int64
	CurrentMessageID int64
	CurrentReceipt   IMAPDraftReceipt
	Revision         int64
	DiscardedAt      *time.Time
	Pending          *IMAPDraftPending
}

var (
	ErrIMAPDraftNotFound = errors.New("IMAP draft not found")
	ErrIMAPDraftRevision = errors.New("IMAP draft revision mismatch")
	ErrIMAPDraftPending  = errors.New("IMAP draft has a pending operation")
	ErrIMAPDraftState    = errors.New("invalid IMAP draft state")
)

// PersistIMAPDraftContext commits the local message snapshot, exact mailbox
// membership, and managed ownership in one transaction.
func (s *Store) PersistIMAPDraftContext(
	ctx context.Context,
	receipt IMAPDraftReceipt,
	participants []ParticipantPersistData,
	build func([]int64) *MessagePersistData,
) (IMAPDraft, error) {
	if err := validateIMAPDraftReceipt(receipt); err != nil {
		return IMAPDraft{}, err
	}
	if build == nil {
		return IMAPDraft{}, errors.New("persist IMAP draft requires a message builder")
	}
	draftID, err := newIMAPDraftID()
	if err != nil {
		return IMAPDraft{}, err
	}
	var draft IMAPDraft
	before := func(ctx context.Context, tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, receipt.SourceID).Scan(&sourceType); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("invalid_source")
			}
			return fmt.Errorf("read IMAP draft source: %w", err)
		}
		if sourceType != "imap" {
			return errors.New("invalid_source")
		}
		var existing int64
		err := tx.QueryRowContext(ctx, `
			SELECT message_id FROM imap_message_memberships
			WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
		`, receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID).Scan(&existing)
		if err == nil {
			return errors.New("source_key_conflict")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check IMAP draft membership: %w", err)
		}
		if err := invalidatePreviousIMAPDraftSourceKey(ctx, tx, receipt); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
		`, receipt.SourceID, IMAPDraftSourceMessageID(receipt)).Scan(&existing)
		if err == nil {
			return errors.New("source_key_conflict")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check IMAP draft source key: %w", err)
		}
		return nil
	}
	prepare := func(ctx context.Context, _ *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
		if data == nil || data.Message == nil {
			return nil, errors.New("persist IMAP draft requires a message")
		}
		if data.Message.SourceID != receipt.SourceID || data.Message.SourceMessageID != IMAPDraftSourceMessageID(receipt) {
			return nil, errors.New("source_key_conflict")
		}
		if data.MIMEAttachmentReplacement != nil {
			return nil, errors.New("IMAP drafts cannot contain attachments")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return data, nil
	}
	after := func(ctx context.Context, tx *loggedTx, data *MessagePersistData, id int64) error {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO imap_message_memberships
				(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
			VALUES (?, ?, ?, ?, ?, %s, %s)
		`, s.dialect.JSONBindExpr(), s.dialect.Now()), receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID, id, imapDraftFlagsJSON); err != nil {
			return fmt.Errorf("persist IMAP draft membership: %w", err)
		}
		labelID, err := ensureIMAPMailboxLabel(ctx, tx, receipt.SourceID, receipt.Mailbox)
		if err != nil {
			return err
		}
		if err := replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, id, []int64{labelID}); err != nil {
			return fmt.Errorf("persist IMAP draft label: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO imap_drafts (
				draft_id, source_id, current_message_id, current_mailbox,
				current_uidvalidity, current_uid, revision, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 1, %s, %s)
		`, s.dialect.Now(), s.dialect.Now()), draftID, receipt.SourceID, id,
			receipt.Mailbox, receipt.UIDValidity, receipt.UID); err != nil {
			return fmt.Errorf("persist IMAP draft ownership: %w", err)
		}
		draft = IMAPDraft{
			DraftID: draftID, SourceID: receipt.SourceID, CurrentMessageID: id,
			CurrentReceipt: receipt, Revision: 1,
		}
		return nil
	}
	if _, err := s.persistMessageWithParticipantsTransaction(ctx, before, participants, build, prepare, after); err != nil {
		return IMAPDraft{}, err
	}
	return draft, nil
}

// APPEND can reuse a key after a folder epoch reset. Keep the old
// archive row, but free the provider key before inserting the new one.
func invalidatePreviousIMAPDraftSourceKey(ctx context.Context, tx *loggedTx, receipt IMAPDraftReceipt) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET source_message_id = 'msgvault-invalidated:' || CAST(id AS TEXT)
		WHERE source_id = ? AND source_message_id = ? AND (EXISTS (
			SELECT 1 FROM imap_message_memberships
			WHERE message_id = messages.id AND source_id = messages.source_id
			  AND mailbox = ? AND uid = ? AND uidvalidity <> ?
		) OR (EXISTS (
			SELECT 1 FROM imap_folder_state WHERE source_id = messages.source_id AND mailbox = ? AND uidvalidity <> ?
		) AND NOT EXISTS (
			SELECT 1 FROM imap_message_memberships WHERE source_id = messages.source_id AND message_id = messages.id AND mailbox = ? AND uid = ?
		)) OR (deleted_from_source_at IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM imap_message_memberships WHERE source_id = messages.source_id AND message_id = messages.id
		)))
	`, receipt.SourceID, IMAPDraftSourceMessageID(receipt), receipt.Mailbox, receipt.UID, receipt.UIDValidity,
		receipt.Mailbox, receipt.UIDValidity, receipt.Mailbox, receipt.UID); err != nil {
		return fmt.Errorf("invalidate previous IMAP draft source key: %w", err)
	}
	return nil
}

func validateIMAPDraftReceipt(receipt IMAPDraftReceipt) error {
	if receipt.SourceID <= 0 || strings.TrimSpace(receipt.Mailbox) == "" || receipt.UID == 0 || receipt.UIDValidity == 0 {
		return errors.New("invalid IMAP draft receipt")
	}
	return nil
}

func newIMAPDraftID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate IMAP draft ID: %w", err)
	}
	return "draft-" + hex.EncodeToString(raw[:]), nil
}

// IMAPDraftSourceMessageID returns the composite provider key used by sync.
func IMAPDraftSourceMessageID(receipt IMAPDraftReceipt) string {
	return fmt.Sprintf("%s|%d", receipt.Mailbox, receipt.UID)
}
