package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const imapDraftFlagsJSON = `["\\Draft"]`

// IMAPDraftReceipt identifies the provider message created by APPEND.
type IMAPDraftReceipt struct {
	SourceID    int64
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// PersistIMAPDraftContext commits the local message snapshot and its exact
// mailbox membership in one transaction. It never changes sync cursors.
func (s *Store) PersistIMAPDraftContext(
	ctx context.Context,
	receipt IMAPDraftReceipt,
	participants []ParticipantPersistData,
	build func([]int64) *MessagePersistData,
) (int64, error) {
	if receipt.SourceID <= 0 || strings.TrimSpace(receipt.Mailbox) == "" || receipt.UID == 0 || receipt.UIDValidity == 0 {
		return 0, errors.New("invalid IMAP draft receipt")
	}
	if build == nil {
		return 0, errors.New("persist IMAP draft requires a message builder")
	}
	var messageID int64
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
		// A new epoch can reuse an archived UID. Preserve the old message under
		// sync's invalidated key; sync still owns membership retirement and cursors.
		// This also retires source-deleted orphan rows left by older versions.
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET source_message_id = 'msgvault-invalidated:' || CAST(id AS TEXT)
			WHERE source_id = ? AND source_message_id = ? AND (
				EXISTS (
					SELECT 1 FROM imap_message_memberships
					WHERE message_id = messages.id AND source_id = messages.source_id
					  AND mailbox = ? AND uid = ? AND uidvalidity <> ?
				) OR EXISTS (
					SELECT 1 FROM imap_folder_state
					WHERE source_id = messages.source_id AND mailbox = ?
					  AND uidvalidity <> ?
				) OR (
					messages.deleted_from_source_at IS NOT NULL
					AND NOT EXISTS (
						SELECT 1 FROM imap_message_memberships
						WHERE message_id = messages.id AND source_id = messages.source_id
					)
				)
			)
		`, receipt.SourceID, IMAPDraftSourceMessageID(receipt), receipt.Mailbox, receipt.UID, receipt.UIDValidity,
			receipt.Mailbox, receipt.UIDValidity); err != nil {
			return fmt.Errorf("invalidate previous IMAP draft source key: %w", err)
		}
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM messages
			WHERE source_id = ? AND source_message_id = ?
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
	after := func(ctx context.Context, tx *loggedTx, _ *MessagePersistData, id int64) error {
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
		if err := s.registerIMAPDraftTx(ctx, tx, receipt, id); err != nil {
			return err
		}
		messageID = id
		return nil
	}
	if _, err := s.persistMessageWithParticipantsTransaction(ctx, before, participants, build, prepare, after); err != nil {
		return 0, err
	}
	return messageID, nil
}

// IMAPDraftSourceMessageID returns the composite provider key used by sync.
func IMAPDraftSourceMessageID(receipt IMAPDraftReceipt) string {
	return fmt.Sprintf("%s|%d", receipt.Mailbox, receipt.UID)
}
