package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/mime"
)

// PersistIMAPRelocationWithParticipantsContext atomically adopts a fetched
// IMAP snapshot under a new source message ID while preserving the guarded
// archive row's internal ID.
func (s *Store) PersistIMAPRelocationWithParticipantsContext(
	ctx context.Context,
	expected MessageIdentityGuard,
	participants []ParticipantPersistData,
	build func(participantIDs []int64) *MessagePersistData,
	replaceLabels bool,
) (int64, error) {
	if build == nil {
		return 0, errors.New("persist IMAP relocation requires a participant builder")
	}
	if s.syncGeneration == nil {
		return 0, errors.New("persist IMAP relocation requires a sync generation")
	}
	if err := s.requireSyncSource(expected.SourceID); err != nil {
		return 0, err
	}

	var expectedRFC822MessageID string
	beforeParticipants := func(ctx context.Context, tx *loggedTx) error {
		var actual MessageIdentityGuard
		var sourceType string
		var storedRFC822MessageID sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT m.id, m.source_id, m.source_message_id,
			       m.rfc822_message_id, s.source_type
			FROM messages m
			JOIN sources s ON s.id = m.source_id
			WHERE m.id = ?`+s.dialect.SelectForUpdate(), expected.ID,
		).Scan(
			&actual.ID, &actual.SourceID, &actual.SourceMessageID,
			&storedRFC822MessageID, &sourceType,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("IMAP relocation identity guard mismatch: target id %d not found", expected.ID)
		}
		if err != nil {
			return fmt.Errorf("read IMAP relocation identity guard: %w", err)
		}
		if actual != expected {
			return fmt.Errorf(
				"IMAP relocation identity guard mismatch: expected (%d, %d, %q), found (%d, %d, %q)",
				expected.ID, expected.SourceID, expected.SourceMessageID,
				actual.ID, actual.SourceID, actual.SourceMessageID,
			)
		}
		if sourceType != "imap" {
			return fmt.Errorf("IMAP relocation requires an IMAP source, found %q", sourceType)
		}
		expectedRFC822MessageID = mime.NormalizeMessageID(storedRFC822MessageID.String)
		if !storedRFC822MessageID.Valid || expectedRFC822MessageID == "" {
			return errors.New("IMAP relocation target requires an RFC822 Message-ID")
		}
		return nil
	}

	var preserveLabels bool
	prepare := func(
		ctx context.Context, tx *loggedTx, data *MessagePersistData,
	) (*MessagePersistData, error) {
		if data == nil || data.Message == nil {
			return nil, errors.New("persist IMAP relocation requires a message")
		}
		if data.Message.SourceID != expected.SourceID {
			return nil, fmt.Errorf(
				"IMAP relocation snapshot source mismatch: expected %d, got %d",
				expected.SourceID, data.Message.SourceID,
			)
		}
		if data.Message.SourceMessageID == "" {
			return nil, errors.New("IMAP relocation snapshot requires a source message ID")
		}
		if data.Message.SourceMessageID == expected.SourceMessageID {
			return nil, errors.New("IMAP relocation snapshot source message ID must change")
		}
		actualRFC822MessageID := mime.NormalizeMessageID(data.Message.RFC822MessageID.String)
		if !data.Message.RFC822MessageID.Valid || actualRFC822MessageID == "" ||
			actualRFC822MessageID != expectedRFC822MessageID {
			return nil, fmt.Errorf(
				"IMAP relocation RFC822 identity mismatch: expected %q, got %q",
				expectedRFC822MessageID, actualRFC822MessageID,
			)
		}
		if len(data.RawMIME) == 0 {
			return nil, errors.New("IMAP relocation snapshot requires raw MIME")
		}
		if data.FTS == nil {
			return nil, errors.New("IMAP relocation snapshot requires FTS content")
		}
		if data.MIMEAttachmentReplacement == nil {
			return nil, errors.New("IMAP relocation snapshot requires MIME attachment replacement")
		}
		if err := validateIMAPRelocationRecipients(data.Recipients); err != nil {
			return nil, err
		}

		result, err := tx.ExecContext(ctx, `
			UPDATE messages
			SET source_message_id = ?
			WHERE id = ? AND source_id = ? AND source_message_id = ?
		`, data.Message.SourceMessageID, expected.ID, expected.SourceID, expected.SourceMessageID)
		if err != nil {
			return nil, fmt.Errorf("rekey IMAP relocation target: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("inspect IMAP relocation rekey: %w", err)
		}
		if rowsAffected != 1 {
			return nil, fmt.Errorf(
				"IMAP relocation identity guard mismatch: rekey affected %d rows", rowsAffected,
			)
		}

		preserveLabels = data.PreserveLabels
		prepared := *data
		message := *data.Message
		prepared.Message = &message
		prepared.PreserveLabels = true
		return &prepared, nil
	}

	afterPersist := func(
		ctx context.Context, tx *loggedTx, data *MessagePersistData, messageID int64,
	) error {
		if messageID != expected.ID {
			return fmt.Errorf(
				"IMAP relocation identity guard mismatch: persistence returned id %d, expected %d",
				messageID, expected.ID,
			)
		}
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.replaceMIMEAttachmentsWith(q, messageID, data.MIMEAttachmentReplacement); err != nil {
			return fmt.Errorf("replace IMAP relocation attachments: %w", err)
		}
		if err := recomputeMessageAttachmentStatsWith(q, messageID); err != nil {
			return fmt.Errorf("recompute IMAP relocation attachment stats: %w", err)
		}
		if !preserveLabels {
			if _, err := s.reconcileMessageLabelsTxContext(
				ctx, tx, messageID, data.LabelIDs, replaceLabels,
			); err != nil {
				return fmt.Errorf("reconcile IMAP relocation labels: %w", err)
			}
		}
		return nil
	}

	return s.persistMessageWithParticipantsTransaction(
		ctx, beforeParticipants, participants, build, prepare, afterPersist,
	)
}

func validateIMAPRelocationRecipients(recipients []RecipientSet) error {
	required := map[string]int{
		"from": 0,
		"to":   0,
		"cc":   0,
		"bcc":  0,
	}
	for _, recipient := range recipients {
		count, ok := required[recipient.Type]
		if !ok {
			return fmt.Errorf("IMAP relocation snapshot has unexpected recipient role %q", recipient.Type)
		}
		required[recipient.Type] = count + 1
	}
	for _, role := range []string{"from", "to", "cc", "bcc"} {
		if required[role] != 1 {
			return fmt.Errorf(
				"IMAP relocation snapshot requires exactly one %s recipient set, got %d",
				role, required[role],
			)
		}
	}
	return nil
}
