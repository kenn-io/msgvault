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
	DraftID                int64
	SourceID               int64
	CurrentMessageID       int64
	Mailbox                string
	UIDValidity            uint32
	UID                    uint32
	Revision               int64
	Lifecycle              string
	PendingKind            sql.NullString
	PendingUIDValidity     sql.NullInt64
	PendingUID             sql.NullInt64
	PendingRaw             []byte
	PendingRfc822ID        sql.NullString
	PendingAppendAttempted bool
	PendingStartedAt       sql.NullTime
	// Projected from the current message row
	ConversationID  int64
	RFC822MessageID string
	ParentMessageID sql.NullString
	FromAddress     string
	Subject         string
	SizeEstimate    int64
}

// IMAPDraftIntent holds the parameters for beginning a draft operation.
type IMAPDraftIntent struct {
	DraftID          int64
	ExpectedRevision int64
	Kind             string // "edit" or "discard"
	PendingRaw       []byte // set for edit, nil for discard
	PendingRfc822ID  string // set for edit, empty for discard
}

// IMAPDraftOutcome holds the result of completing a draft operation.
type IMAPDraftOutcome struct {
	Lifecycle string // "active" (after replace) or "discarded" (after delete)
	// For the membership to delete (the old copy):
	SourceID    int64
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	// The message row whose membership was just removed:
	MessageID int64
}

// GetIMAPDraftContext loads the ownership row for one draft, joining the
// current message for projected fields. sql.ErrNoRows → opserr.NotFound.
func (s *Store) GetIMAPDraftContext(ctx context.Context, draftID int64) (*IMAPDraft, error) {
	var d IMAPDraft
	var uidValidity, uid int64
	var parentRFC822 sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT
			d.draft_id, d.source_id, d.current_message_id,
			d.mailbox, d.uidvalidity, d.uid, d.revision, d.lifecycle,
			d.pending_kind, d.pending_uidvalidity, d.pending_uid,
			d.pending_raw, d.pending_rfc822_id, d.pending_append_attempted,
			d.pending_started_at,
			m.conversation_id,
			COALESCE(m.rfc822_message_id, ''),
			pm.rfc822_message_id,
			COALESCE(p.email_address, ''),
			COALESCE(m.subject, ''),
			COALESCE(m.size_estimate, 0)
		FROM imap_drafts d
		JOIN messages m ON m.id = d.current_message_id
		LEFT JOIN messages pm ON pm.id = m.reply_to_message_id
		LEFT JOIN participants p ON p.id = m.sender_id
		WHERE d.draft_id = ?
		  AND `+LiveMessagesWhere("m", false),
	), draftID).Scan(
		&d.DraftID, &d.SourceID, &d.CurrentMessageID,
		&d.Mailbox, &uidValidity, &uid, &d.Revision, &d.Lifecycle,
		&d.PendingKind, &d.PendingUIDValidity, &d.PendingUID,
		&d.PendingRaw, &d.PendingRfc822ID, &d.PendingAppendAttempted,
		&d.PendingStartedAt,
		&d.ConversationID,
		&d.RFC822MessageID, &parentRFC822, &d.FromAddress, &d.Subject, &d.SizeEstimate,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, opserr.NotFound(fmt.Errorf("draft %d: not found", draftID))
	}
	if err != nil {
		return nil, fmt.Errorf("get IMAP draft %d: %w", draftID, err)
	}
	d.UIDValidity = uint32(uidValidity)
	d.UID = uint32(uid)
	d.ParentMessageID = parentRFC822
	return &d, nil
}

// BeginIMAPDraftOperationContext claims the draft for a pending mutation using
// a CAS update. It returns the current draft state (with pending fields set) on
// success. On RowsAffected==0 it inspects why and returns either
// "revision_conflict" or "operation_pending" wrapped in opserr.Invalid.
func (s *Store) BeginIMAPDraftOperationContext(ctx context.Context, intent IMAPDraftIntent) (*IMAPDraft, error) {
	now := s.dialect.Now()
	newLifecycle := "replace_pending"
	if intent.Kind == "discard" {
		newLifecycle = "delete_pending"
	}
	var pendingRaw interface{}
	if len(intent.PendingRaw) > 0 {
		pendingRaw = intent.PendingRaw
	}
	var pendingRfc822ID interface{}
	if intent.PendingRfc822ID != "" {
		pendingRfc822ID = intent.PendingRfc822ID
	}

	result, err := s.db.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE imap_drafts
		SET lifecycle = ?,
		    pending_kind = ?,
		    pending_raw = ?,
		    pending_rfc822_id = ?,
		    pending_append_attempted = FALSE,
		    pending_uid = NULL,
		    pending_uidvalidity = NULL,
		    pending_started_at = %s,
		    revision = revision + 1,
		    updated_at = %s
		WHERE draft_id = ?
		  AND lifecycle = 'active'
		  AND revision = ?
	`, now, now)),
		newLifecycle, intent.Kind, pendingRaw, pendingRfc822ID,
		intent.DraftID, intent.ExpectedRevision,
	)
	if err != nil {
		return nil, fmt.Errorf("begin IMAP draft operation %d: %w", intent.DraftID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("begin IMAP draft operation rows affected %d: %w", intent.DraftID, err)
	}
	if n == 1 {
		return s.GetIMAPDraftContext(ctx, intent.DraftID)
	}
	// Zero rows: determine why.
	var lifecycle string
	var revision int64
	err = s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT lifecycle, revision FROM imap_drafts WHERE draft_id = ?
	`), intent.DraftID).Scan(&lifecycle, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, opserr.NotFound(fmt.Errorf("draft %d: not found", intent.DraftID))
	}
	if err != nil {
		return nil, fmt.Errorf("inspect IMAP draft %d after CAS miss: %w", intent.DraftID, err)
	}
	if lifecycle != "active" {
		return nil, opserr.Invalid(errors.New("operation_pending"))
	}
	return nil, opserr.Invalid(errors.New("revision_conflict"))
}

// RecordIMAPDraftAppendContext records the outcome of an APPEND attempt.
// First it marks pending_append_attempted=TRUE. If receipt is non-nil it also
// records the accepted (uidvalidity, uid) pair.
func (s *Store) RecordIMAPDraftAppendContext(ctx context.Context, draftID, expectedRevision int64, receipt *IMAPDraftReceipt) error {
	now := s.dialect.Now()
	if _, err := s.db.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE imap_drafts
		SET pending_append_attempted = TRUE, updated_at = %s
		WHERE draft_id = ? AND revision = ?
	`, now)), draftID, expectedRevision); err != nil {
		return fmt.Errorf("record IMAP draft append attempted %d: %w", draftID, err)
	}
	if receipt == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE imap_drafts
		SET pending_uidvalidity = ?, pending_uid = ?, updated_at = %s
		WHERE draft_id = ? AND revision = ?
	`, now)), int64(receipt.UIDValidity), int64(receipt.UID), draftID, expectedRevision); err != nil {
		return fmt.Errorf("record IMAP draft append receipt %d: %w", draftID, err)
	}
	return nil
}

// PersistIMAPDraftReplacementContext archives the new message body for a draft
// edit. The after hook CAS-updates imap_drafts to point to the new message.
func (s *Store) PersistIMAPDraftReplacementContext(
	ctx context.Context,
	draftID, expectedRevision int64,
	receipt IMAPDraftReceipt,
	participants []ParticipantPersistData,
	build func([]int64) *MessagePersistData,
) (int64, error) {
	if build == nil {
		return 0, errors.New("persist IMAP draft replacement requires a message builder")
	}
	var newMessageID int64
	before := func(ctx context.Context, tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT source_type FROM sources WHERE id = ?`), receipt.SourceID).Scan(&sourceType); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("invalid_source")
			}
			return fmt.Errorf("read IMAP draft replacement source: %w", err)
		}
		if sourceType != "imap" {
			return errors.New("invalid_source")
		}
		return nil
	}
	prepare := func(ctx context.Context, _ *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
		if data == nil || data.Message == nil {
			return nil, errors.New("persist IMAP draft replacement requires a message")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return data, nil
	}
	after := func(ctx context.Context, tx *loggedTx, _ *MessagePersistData, id int64) error {
		// Insert new membership row.
		if _, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			INSERT INTO imap_message_memberships
				(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
			VALUES (?, ?, ?, ?, ?, %s, %s)
		`, s.dialect.JSONBindExpr(), s.dialect.Now())),
			receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID, id, imapDraftFlagsJSON); err != nil {
			return fmt.Errorf("persist IMAP draft replacement membership: %w", err)
		}
		// Write labels.
		labelID, err := ensureIMAPMailboxLabel(ctx, tx, receipt.SourceID, receipt.Mailbox)
		if err != nil {
			return err
		}
		if err := replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, id, []int64{labelID}); err != nil {
			return fmt.Errorf("persist IMAP draft replacement label: %w", err)
		}
		// CAS update imap_drafts to point to the new message.
		result, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			UPDATE imap_drafts
			SET current_message_id = ?,
			    uidvalidity = ?,
			    uid = ?,
			    updated_at = %s
			WHERE draft_id = ? AND revision = ?
		`, s.dialect.Now())),
			id, int64(receipt.UIDValidity), int64(receipt.UID),
			draftID, expectedRevision,
		)
		if err != nil {
			return fmt.Errorf("persist IMAP draft replacement CAS: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("persist IMAP draft replacement CAS rows: %w", err)
		}
		if n != 1 {
			return opserr.Invalid(errors.New("revision_conflict"))
		}
		if err := s.bumpDerivedDataRevision(tx); err != nil {
			return err
		}
		newMessageID = id
		return nil
	}
	if _, err := s.persistMessageWithParticipantsTransaction(ctx, before, participants, build, prepare, after); err != nil {
		return 0, err
	}
	return newMessageID, nil
}

// FinishIMAPDraftOperationContext completes a pending edit or discard:
// 1. Deletes the specific old membership row.
// 2. Rebuilds labels for the old message.
// 3. Tombstones the old message if it has no remaining memberships.
// 4. CAS-updates imap_drafts lifecycle and clears all pending columns.
// 5. Bumps the derived-data revision.
func (s *Store) FinishIMAPDraftOperationContext(ctx context.Context, draftID, expectedRevision int64, outcome IMAPDraftOutcome) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		// 1. Delete the exact old membership row.
		if _, err := tx.ExecContext(ctx, s.Rebind(`
			DELETE FROM imap_message_memberships
			WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
		`), outcome.SourceID, outcome.Mailbox, int64(outcome.UIDValidity), int64(outcome.UID)); err != nil {
			return fmt.Errorf("finish IMAP draft operation delete membership: %w", err)
		}

		// 2. Rebuild labels for the old message.
		var mailboxes []string
		rows, err := tx.QueryContext(ctx, s.Rebind(`
			SELECT mailbox FROM imap_message_memberships
			WHERE source_id = ? AND message_id = ?
			GROUP BY mailbox
		`), outcome.SourceID, outcome.MessageID)
		if err != nil {
			return fmt.Errorf("finish IMAP draft operation list memberships: %w", err)
		}
		for rows.Next() {
			var mb string
			if err := rows.Scan(&mb); err != nil {
				_ = rows.Close()
				return fmt.Errorf("finish IMAP draft operation scan mailbox: %w", err)
			}
			mailboxes = append(mailboxes, mb)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("finish IMAP draft operation close mailbox rows: %w", err)
		}
		var labelIDs []int64
		for _, mb := range mailboxes {
			labelID, err := ensureIMAPMailboxLabel(ctx, tx, outcome.SourceID, mb)
			if err != nil {
				return err
			}
			labelIDs = append(labelIDs, labelID)
		}
		if err := replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, outcome.MessageID, labelIDs); err != nil {
			return fmt.Errorf("finish IMAP draft operation replace labels: %w", err)
		}

		// 3. Tombstone old message if membership-free.
		if len(mailboxes) == 0 {
			if _, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
				UPDATE messages SET deleted_from_source_at = %s
				WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL
			`, s.dialect.Now())), outcome.MessageID, outcome.SourceID); err != nil {
				return fmt.Errorf("finish IMAP draft operation tombstone message %d: %w", outcome.MessageID, err)
			}
		}

		// 4. CAS-update imap_drafts lifecycle and clear pending columns.
		result, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			UPDATE imap_drafts
			SET lifecycle = ?,
			    pending_kind = NULL,
			    pending_uidvalidity = NULL,
			    pending_uid = NULL,
			    pending_raw = NULL,
			    pending_rfc822_id = NULL,
			    pending_append_attempted = FALSE,
			    pending_started_at = NULL,
			    updated_at = %s
			WHERE draft_id = ? AND revision = ?
		`, s.dialect.Now())),
			outcome.Lifecycle, draftID, expectedRevision,
		)
		if err != nil {
			return fmt.Errorf("finish IMAP draft operation CAS lifecycle: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("finish IMAP draft operation CAS lifecycle rows: %w", err)
		}
		if n != 1 {
			return opserr.Invalid(errors.New("revision_conflict"))
		}

		// 5. Bump derived-data revision.
		return s.bumpDerivedDataRevision(tx)
	})
}

// registerIMAPDraftTx inserts the ownership row for a newly created draft.
// Called from PersistIMAPDraftContext's after hook.
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
