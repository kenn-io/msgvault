package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/opserr"
)

// IMAPDraft holds the ownership row for one draft msgvault created.
type IMAPDraft struct {
	DraftID          int64
	SourceID         int64
	CurrentMessageID int64
	Mailbox          string
	UIDValidity      uint32
	UID              uint32
	Revision         int64
	Lifecycle        string
	// Projected from the current message row.
	ConversationID   int64
	RFC822MessageID  string
	ParentMessageID  sql.NullString
	ReplyToMessageID sql.NullInt64
	FromAddress      string
	Subject          string
	Snippet          string
	SizeEstimate     int64
}

// GetIMAPDraftContext loads the ownership row, the current message, and its
// current mailbox membership. It does not change local state.
func (s *Store) GetIMAPDraftContext(ctx context.Context, draftID int64) (*IMAPDraft, error) {
	var draft IMAPDraft
	var uidValidity, uid int64
	var parentRFC822 sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		WITH current_receipt AS (
			SELECT d2.draft_id, mm.mailbox, mm.uidvalidity, mm.uid
			FROM imap_drafts d2
			JOIN imap_message_memberships mm
			  ON mm.source_id = d2.source_id
			 AND mm.message_id = d2.current_message_id
			WHERE d2.draft_id = ?
			ORDER BY
				CASE WHEN mm.mailbox = d2.mailbox
				       AND mm.uidvalidity = d2.uidvalidity
				       AND mm.uid = d2.uid THEN 0 ELSE 1 END,
				mm.updated_at DESC, mm.mailbox, mm.uidvalidity, mm.uid
			LIMIT 1
		)
		SELECT
			d.draft_id, d.source_id, d.current_message_id,
			COALESCE(cr.mailbox, d.mailbox),
			COALESCE(cr.uidvalidity, d.uidvalidity),
			COALESCE(cr.uid, d.uid),
			d.revision, d.lifecycle,
			m.conversation_id,
			COALESCE(m.rfc822_message_id, ''),
			pm.rfc822_message_id,
			m.reply_to_message_id,
			COALESCE(p.email_address, ''),
			COALESCE(m.subject, ''),
			COALESCE(m.snippet, ''),
			COALESCE(m.size_estimate, 0)
		FROM imap_drafts d
		JOIN messages m ON m.id = d.current_message_id
		LEFT JOIN current_receipt cr ON cr.draft_id = d.draft_id
		LEFT JOIN messages pm ON pm.id = m.reply_to_message_id
		LEFT JOIN participants p ON p.id = m.sender_id
		WHERE d.draft_id = ?
		`,
	), draftID, draftID).Scan(
		&draft.DraftID, &draft.SourceID, &draft.CurrentMessageID,
		&draft.Mailbox, &uidValidity, &uid, &draft.Revision, &draft.Lifecycle,
		&draft.ConversationID,
		&draft.RFC822MessageID, &parentRFC822, &draft.ReplyToMessageID,
		&draft.FromAddress, &draft.Subject, &draft.Snippet, &draft.SizeEstimate,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, opserr.NotFound(fmt.Errorf("draft %d: not found", draftID))
	}
	if err != nil {
		return nil, fmt.Errorf("get IMAP draft %d: %w", draftID, err)
	}
	if uidValidity < 0 || uidValidity > int64(^uint32(0)) || uid < 0 || uid > int64(^uint32(0)) {
		return nil, fmt.Errorf("get IMAP draft %d: UID receipt is out of range", draftID)
	}
	draft.UIDValidity = uint32(uidValidity)
	draft.UID = uint32(uid)
	draft.ParentMessageID = parentRFC822
	return &draft, nil
}

// registerIMAPDraftTx inserts the ownership row for a newly created draft.
// PersistIMAPDraftContext calls it in the same transaction as the message and
// membership rows.
func (s *Store) registerIMAPDraftTx(ctx context.Context, tx *loggedTx, receipt IMAPDraftReceipt, messageID int64) error {
	if _, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		INSERT INTO imap_drafts
			(draft_id, source_id, current_message_id, mailbox, uidvalidity, uid,
			 revision, lifecycle, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, 'active', %s, %s)
	`, s.dialect.Now(), s.dialect.Now())),
		messageID, receipt.SourceID, messageID,
		receipt.Mailbox, int64(receipt.UIDValidity), int64(receipt.UID),
	); err != nil {
		return fmt.Errorf("register IMAP draft ownership: %w", err)
	}
	return s.bumpDerivedDataRevision(tx)
}
