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
	DraftID            int64
	SourceID           int64
	CurrentMessageID   int64
	Mailbox            string
	UIDValidity        uint32
	UID                uint32
	Revision           int64
	Lifecycle          string
	PendingKind        sql.NullString
	PendingUIDValidity sql.NullInt64
	PendingUID         sql.NullInt64
	PendingStartedAt   sql.NullTime
	// Projected from the current message row
	ConversationID   int64
	RFC822MessageID  string
	ParentMessageID  sql.NullString
	ReplyToMessageID sql.NullInt64 // integer FK of the parent message (for edit replacement)
	FromAddress      string
	Subject          string
	Snippet          string
	SizeEstimate     int64
}

// IMAPDraftIntent holds the parameters for beginning a draft operation.
type IMAPDraftIntent struct {
	DraftID          int64
	ExpectedRevision int64
	Kind             string // "edit" or "discard"
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

// refreshIMAPDraftReceiptContext follows the current message's membership
// after a mailbox move or UIDVALIDITY reset. The message ID is the stable
// ownership link; mailbox coordinates are provider state and can change.
func (s *Store) refreshIMAPDraftReceiptContext(ctx context.Context, draftID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var sourceID, messageID, oldUIDValidity, oldUID int64
		var oldMailbox string
		var pendingKind sql.NullString
		err := tx.QueryRowContext(ctx, s.Rebind(`
			SELECT source_id, current_message_id, mailbox, uidvalidity, uid, pending_kind
			FROM imap_drafts
			WHERE draft_id = ?
		`), draftID).Scan(&sourceID, &messageID, &oldMailbox, &oldUIDValidity, &oldUID, &pendingKind)
		if errors.Is(err, sql.ErrNoRows) {
			return opserr.NotFound(fmt.Errorf("draft %d: not found", draftID))
		}
		if err != nil {
			return fmt.Errorf("read IMAP draft %d receipt: %w", draftID, err)
		}
		if pendingKind.Valid {
			return nil
		}

		var mailbox string
		var uidValidity, uid int64
		err = tx.QueryRowContext(ctx, s.Rebind(`
			SELECT mailbox, uidvalidity, uid
			FROM imap_message_memberships
			WHERE source_id = ? AND message_id = ?
			ORDER BY
				CASE WHEN mailbox = ? AND uidvalidity = ? AND uid = ? THEN 0 ELSE 1 END,
				updated_at DESC, mailbox, uidvalidity, uid
			LIMIT 1
		`), sourceID, messageID, oldMailbox, oldUIDValidity, oldUID).Scan(&mailbox, &uidValidity, &uid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find IMAP draft %d membership: %w", draftID, err)
		}
		if mailbox == oldMailbox && uidValidity == oldUIDValidity && uid == oldUID {
			return nil
		}

		_, err = tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			UPDATE imap_drafts
			SET mailbox = ?,
			    uidvalidity = ?,
			    uid = ?,
			    pending_uidvalidity = CASE
			        WHEN pending_uidvalidity = ? AND pending_uid = ? THEN ?
			        ELSE pending_uidvalidity
			    END,
			    pending_uid = CASE
			        WHEN pending_uidvalidity = ? AND pending_uid = ? THEN ?
			        ELSE pending_uid
			    END,
			    updated_at = %s
			WHERE draft_id = ?
			  AND lifecycle = 'active'
			  AND current_message_id = ?
			  AND (mailbox <> ? OR uidvalidity <> ? OR uid <> ?)
		`, s.dialect.Now())),
			mailbox, uidValidity, uid,
			oldUIDValidity, oldUID, uidValidity,
			oldUIDValidity, oldUID, uid,
			draftID, messageID, mailbox, uidValidity, uid,
		)
		if err != nil {
			return fmt.Errorf("refresh IMAP draft %d receipt: %w", draftID, err)
		}
		return nil
	})
}

// RefreshIMAPDraftDiscardReceiptContext resolves a pending discard against
// the current membership of the stable draft message. It returns false when
// the archive has no membership that can safely identify the remote copy.
func (s *Store) RefreshIMAPDraftDiscardReceiptContext(ctx context.Context, draftID int64) (bool, error) {
	found := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var sourceID, messageID, oldUIDValidity, oldUID int64
		var oldMailbox string
		var pendingKind sql.NullString
		err := tx.QueryRowContext(ctx, s.Rebind(`
			SELECT source_id, current_message_id, mailbox, uidvalidity, uid, pending_kind
			FROM imap_drafts
			WHERE draft_id = ?
		`), draftID).Scan(&sourceID, &messageID, &oldMailbox, &oldUIDValidity, &oldUID, &pendingKind)
		if errors.Is(err, sql.ErrNoRows) {
			return opserr.NotFound(fmt.Errorf("draft %d: not found", draftID))
		}
		if err != nil {
			return fmt.Errorf("read pending IMAP draft %d receipt: %w", draftID, err)
		}
		if !pendingKind.Valid || pendingKind.String != "discard" {
			return nil
		}

		var mailbox string
		var uidValidity, uid int64
		err = tx.QueryRowContext(ctx, s.Rebind(`
			SELECT mailbox, uidvalidity, uid
			FROM imap_message_memberships
			WHERE source_id = ? AND message_id = ?
			ORDER BY
				CASE WHEN mailbox = ? AND uidvalidity = ? AND uid = ? THEN 0 ELSE 1 END,
				updated_at DESC, mailbox, uidvalidity, uid
			LIMIT 1
		`), sourceID, messageID, oldMailbox, oldUIDValidity, oldUID).Scan(&mailbox, &uidValidity, &uid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find pending IMAP draft %d membership: %w", draftID, err)
		}
		if uidValidity < 0 || uidValidity > int64(^uint32(0)) || uid < 0 || uid > int64(^uint32(0)) {
			return fmt.Errorf("pending IMAP draft %d has an out-of-range UID receipt", draftID)
		}
		found = true
		if mailbox == oldMailbox && uidValidity == oldUIDValidity && uid == oldUID {
			return nil
		}

		_, err = tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			UPDATE imap_drafts
			SET mailbox = ?,
			    uidvalidity = ?,
			    uid = ?,
			    pending_uidvalidity = ?,
			    pending_uid = ?,
			    updated_at = %s
			WHERE draft_id = ?
			  AND current_message_id = ?
			  AND pending_kind = 'discard'
		`, s.dialect.Now())),
			mailbox, uidValidity, uid, uidValidity, uid,
			draftID, messageID,
		)
		if err != nil {
			return fmt.Errorf("refresh pending IMAP draft %d receipt: %w", draftID, err)
		}
		return nil
	})
	return found, err
}

// GetIMAPDraftContext loads the ownership row for one draft, joining the
// current message for projected fields. It refreshes provider coordinates from
// the stable current message before reading them. sql.ErrNoRows → opserr.NotFound.
func (s *Store) GetIMAPDraftContext(ctx context.Context, draftID int64) (*IMAPDraft, error) {
	if err := s.refreshIMAPDraftReceiptContext(ctx, draftID); err != nil {
		return nil, err
	}
	var d IMAPDraft
	var uidValidity, uid int64
	var parentRFC822 sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT
			d.draft_id, d.source_id, d.current_message_id,
			d.mailbox, d.uidvalidity, d.uid, d.revision, d.lifecycle,
			d.pending_kind, d.pending_uidvalidity, d.pending_uid,
			d.pending_started_at,
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
		LEFT JOIN messages pm ON pm.id = m.reply_to_message_id
		LEFT JOIN participants p ON p.id = m.sender_id
		WHERE d.draft_id = ?
		  AND `+LiveMessagesWhere("m", false),
	), draftID).Scan(
		&d.DraftID, &d.SourceID, &d.CurrentMessageID,
		&d.Mailbox, &uidValidity, &uid, &d.Revision, &d.Lifecycle,
		&d.PendingKind, &d.PendingUIDValidity, &d.PendingUID,
		&d.PendingStartedAt,
		&d.ConversationID,
		&d.RFC822MessageID, &parentRFC822, &d.ReplyToMessageID, &d.FromAddress, &d.Subject, &d.Snippet, &d.SizeEstimate,
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
	d.UIDValidity = uint32(uidValidity)
	d.UID = uint32(uid)
	d.ParentMessageID = parentRFC822
	return &d, nil
}

// GetIMAPDraftPendingMessageIDContext returns the archived message whose IMAP
// membership names one draft receipt. An interrupted edit is the case that
// needs it: Finish is what removes the pre-edit copy's membership, so while a
// pending marker is still set that membership is intact and names the message
// whose raw bytes the pre-edit copy carries. Those bytes, not the draft's
// current raw, are what the ownership digest has to be computed over once
// Persist has moved current_message_id to the replacement.
//
// sql.ErrNoRows → opserr.NotFound, which is the ordinary state for a receipt
// whose Finish already ran.
func (s *Store) GetIMAPDraftPendingMessageIDContext(
	ctx context.Context,
	sourceID int64,
	mailbox string,
	uidValidity, uid uint32,
) (int64, error) {
	var messageID int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT message_id FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), sourceID, mailbox, int64(uidValidity), int64(uid)).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, opserr.NotFound(fmt.Errorf(
			"draft receipt %s|%d:%d: no membership", mailbox, uidValidity, uid))
	}
	if err != nil {
		return 0, fmt.Errorf("get IMAP draft receipt message %s|%d:%d: %w", mailbox, uidValidity, uid, err)
	}
	return messageID, nil
}

// BeginIMAPDraftOperationContext claims the draft for a pending mutation using
// a CAS update. It returns the current draft state (with pending fields set) on
// success. On RowsAffected==0 it inspects why and returns "operation_pending"
// for an actual pending operation, "draft_discarded" for a terminal lifecycle,
// or "revision_conflict" for a stale revision, wrapped in opserr.Invalid.
func (s *Store) BeginIMAPDraftOperationContext(ctx context.Context, intent IMAPDraftIntent) (*IMAPDraft, error) {
	now := s.dialect.Now()

	result, err := s.db.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE imap_drafts
		SET pending_kind = ?,
		    pending_uid = uid,
		    pending_uidvalidity = uidvalidity,
		    pending_started_at = %s,
		    revision = revision + 1,
		    updated_at = %s
		WHERE draft_id = ?
		  AND lifecycle = 'active'
		  AND pending_kind IS NULL
		  AND revision = ?
	`, now, now)),
		intent.Kind,
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
	var pendingKind sql.NullString
	err = s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT lifecycle, revision, pending_kind FROM imap_drafts WHERE draft_id = ?
	`), intent.DraftID).Scan(&lifecycle, &revision, &pendingKind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, opserr.NotFound(fmt.Errorf("draft %d: not found", intent.DraftID))
	}
	if err != nil {
		return nil, fmt.Errorf("inspect IMAP draft %d after CAS miss: %w", intent.DraftID, err)
	}
	// operation_pending is reserved for an actual pending operation. A
	// discarded draft is a terminal lifecycle, not a mutation in flight, and
	// a revision that no longer matches is an ordinary reload-and-retry
	// conflict whichever lifecycle the row carries.
	if pendingKind.Valid {
		return nil, opserr.Invalid(errors.New("operation_pending"))
	}
	if revision != intent.ExpectedRevision {
		return nil, opserr.Invalid(errors.New("revision_conflict"))
	}
	if lifecycle != "active" {
		return nil, opserr.Invalid(errors.New("draft_discarded"))
	}
	// Nothing distinguishable remains: a concurrent writer moved the row
	// between the CAS and this read.
	return nil, opserr.Invalid(errors.New("revision_conflict"))
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
		// 1. Find and delete the exact old membership row.
		// Look up the message_id from the membership so callers do not need to
		// track it separately across crash-recovery paths (P1-C).
		var oldMessageID int64
		lookupErr := tx.QueryRowContext(ctx, s.Rebind(`
			SELECT message_id FROM imap_message_memberships
			WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
		`), outcome.SourceID, outcome.Mailbox, int64(outcome.UIDValidity), int64(outcome.UID)).Scan(&oldMessageID)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			// Membership already gone; use the caller-supplied fallback.
			oldMessageID = outcome.MessageID
		} else if lookupErr != nil {
			return fmt.Errorf("finish IMAP draft operation lookup membership: %w", lookupErr)
		}
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
		`), outcome.SourceID, oldMessageID)
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
		if err := replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, oldMessageID, labelIDs); err != nil {
			return fmt.Errorf("finish IMAP draft operation replace labels: %w", err)
		}

		// 3. Tombstone old message if membership-free.
		if len(mailboxes) == 0 && oldMessageID != 0 {
			if _, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
				UPDATE messages SET deleted_from_source_at = %s
				WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL
			`, s.dialect.Now())), oldMessageID, outcome.SourceID); err != nil {
				return fmt.Errorf("finish IMAP draft operation tombstone message %d: %w", oldMessageID, err)
			}
		}

		// 4. CAS-update imap_drafts lifecycle and clear pending columns.
		result, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			UPDATE imap_drafts
			SET lifecycle = ?,
			    pending_kind = NULL,
			    pending_uidvalidity = NULL,
			    pending_uid = NULL,
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

// ClearIMAPDraftPendingEditContext resets an interrupted edit operation by
// clearing the pending_* columns without changing lifecycle. The caller should
// inform the operator that a duplicate may remain in the Drafts mailbox.
func (s *Store) ClearIMAPDraftPendingEditContext(ctx context.Context, draftID int64) error {
	now := s.dialect.Now()
	if _, err := s.db.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE imap_drafts
		SET pending_kind = NULL,
		    pending_uid = NULL,
		    pending_uidvalidity = NULL,
		    pending_started_at = NULL,
		    updated_at = %s
		WHERE draft_id = ? AND pending_kind = 'edit'
	`, now)), draftID); err != nil {
		return fmt.Errorf("clear IMAP draft pending edit %d: %w", draftID, err)
	}
	return nil
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
