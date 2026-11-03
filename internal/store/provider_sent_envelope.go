package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const sourceTypeIMAP = "imap"

func (s *Store) mergeProviderSentEnvelopeEvidenceTx(
	ctx context.Context, tx *loggedTx, survivorID, duplicateID int64,
) error {
	if survivorID == duplicateID {
		return nil
	}
	duplicateSent := s.providerSentEnvelopeEvidenceSQL("duplicate", "duplicate_source")
	survivorSent := s.providerSentEnvelopeEvidenceSQL("survivor", "survivor_source")
	var sourceType string
	var hasDuplicateSent, hasSurvivorSent bool
	err := tx.QueryRowContext(ctx, `SELECT duplicate_source.source_type, `+duplicateSent+`, `+survivorSent+`
		FROM messages duplicate
		JOIN sources duplicate_source ON duplicate_source.id=duplicate.source_id
		JOIN messages survivor ON survivor.id=?
		JOIN sources survivor_source ON survivor_source.id=survivor.source_id
		WHERE duplicate.id=? AND duplicate_source.source_type=survivor_source.source_type
		  AND duplicate_source.source_type IN ('gmail','`+sourceTypeIMAP+`')`, survivorID, duplicateID).
		Scan(&sourceType, &hasDuplicateSent, &hasSurvivorSent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read provider Sent envelope evidence: %w", err)
	}
	if sourceType != "gmail" && sourceType != sourceTypeIMAP {
		return nil
	}
	if hasSurvivorSent {
		if err := s.copyProviderSentEnvelopeTx(ctx, tx, survivorID, survivorID); err != nil {
			return fmt.Errorf("preserve survivor provider Sent envelope: %w", err)
		}
	}
	if hasDuplicateSent {
		if err := s.copyProviderSentEnvelopeTx(ctx, tx, survivorID, duplicateID); err != nil {
			return fmt.Errorf("preserve duplicate provider Sent envelope: %w", err)
		}
	}
	return nil
}

func (s *Store) providerSentEnvelopeEvidenceSQL(message, source string) string {
	origin := message + ".metadata ->> 'imap_content_origin'"
	if !s.IsPostgreSQL() {
		origin = "CASE WHEN json_valid(" + message + ".metadata) THEN " + message + ".metadata ->> 'imap_content_origin' END"
	}
	return `(EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
		WHERE ml.message_id=` + message + `.id AND l.source_id=` + message + `.source_id
		  AND (l.system_role='` + LabelSystemRoleSent + `' OR (` + source + `.source_type='gmail' AND l.source_label_id='SENT'))) OR
		(` + source + `.source_type='` + sourceTypeIMAP + `' AND COALESCE(` + origin + `,'')='sent') OR
		EXISTS (SELECT 1 FROM provider_sent_envelope_recipients sent WHERE sent.message_id=` + message + `.id))`
}

func (s *Store) copyProviderSentEnvelopeTx(ctx context.Context, tx *loggedTx, targetID, sentMessageID int64) error {
	var sourceHasSnapshot bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM provider_sent_envelope_recipients WHERE message_id=?
	)`, sentMessageID).Scan(&sourceHasSnapshot); err != nil {
		return fmt.Errorf("check provider Sent envelope snapshot: %w", err)
	}
	if sourceHasSnapshot {
		insert := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO provider_sent_envelope_recipients
			(message_id, email_address)
			SELECT ?, email_address FROM provider_sent_envelope_recipients WHERE message_id=?`)
		if _, err := tx.ExecContext(ctx, insert, targetID, sentMessageID); err != nil {
			return fmt.Errorf("copy provider Sent envelope snapshot: %w", err)
		}
		return nil
	}
	insert := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO provider_sent_envelope_recipients
		(message_id, email_address)
		SELECT ?, LOWER(COALESCE(TRIM(email_address), ''))
		FROM message_recipients
		WHERE message_id=? AND recipient_type IN ('to','cc','bcc')
		  AND LOWER(COALESCE(TRIM(email_address), ''))<>''`)
	if _, err := tx.ExecContext(ctx, insert, targetID, sentMessageID); err != nil {
		return fmt.Errorf("copy provider Sent envelope recipients: %w", err)
	}
	return nil
}
