package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

const sourceTypeIMAP = "imap"

const microsoftSentEnvelopeObservedKey = "msmail_sent_envelope_observed"

const providerSentEnvelopeObservedKey = "provider_sent_envelope_observed"

func metadataFlagSQL(dialect Dialect, message, key string) string {
	if dialect.DriverName() == postgresDriverName {
		return `COALESCE(` + message + `.metadata ->> '` + key + `'='true', FALSE)`
	}
	return `COALESCE(CASE WHEN json_valid(` + message + `.metadata) THEN json_extract(` + message + `.metadata, '$.` + key + `') END=1, FALSE)`
}

func microsoftSentEnvelopeSnapshotExistsSQL(dialect Dialect, message string) string {
	return `(` + metadataFlagSQL(dialect, message, microsoftSentEnvelopeObservedKey) + ` OR EXISTS (
		SELECT 1 FROM msmail_sent_envelope_recipients sent WHERE sent.message_id=` + message + `.id))`
}

func providerSentEnvelopeSnapshotExistsSQL(dialect Dialect, message string) string {
	return `(` + metadataFlagSQL(dialect, message, providerSentEnvelopeObservedKey) + ` OR EXISTS (
		SELECT 1 FROM provider_sent_envelope_recipients sent WHERE sent.message_id=` + message + `.id))`
}

func senderSentEnvelopeSnapshotExistsSQL(dialect Dialect, message string) string {
	return `(` + microsoftSentEnvelopeSnapshotExistsSQL(dialect, message) + ` OR ` +
		providerSentEnvelopeSnapshotExistsSQL(dialect, message) + `)`
}

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
		` + providerSentEnvelopeSnapshotExistsSQL(s.dialect, message) + `)`
}

func (s *Store) copyProviderSentEnvelopeTx(ctx context.Context, tx *loggedTx, targetID, sentMessageID int64) error {
	var sourceHasSnapshot bool
	if err := tx.QueryRowContext(ctx, `SELECT `+providerSentEnvelopeSnapshotExistsSQL(s.dialect, "m")+
		` FROM messages m WHERE m.id=?`, sentMessageID).Scan(&sourceHasSnapshot); err != nil {
		return fmt.Errorf("check provider Sent envelope snapshot: %w", err)
	}
	if sourceHasSnapshot {
		insert := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO provider_sent_envelope_recipients
			(message_id, email_address)
			SELECT ?, email_address FROM provider_sent_envelope_recipients WHERE message_id=?`)
		if _, err := tx.ExecContext(ctx, insert, targetID, sentMessageID); err != nil {
			return fmt.Errorf("copy provider Sent envelope snapshot: %w", err)
		}
	} else {
		insert := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO provider_sent_envelope_recipients
			(message_id, email_address)
		SELECT ?, LOWER(COALESCE(TRIM(email_address), ''))
		FROM message_recipients
		WHERE message_id=? AND recipient_type IN ('to','cc','bcc')
		  AND LOWER(COALESCE(TRIM(email_address), ''))<>''`)
		if _, err := tx.ExecContext(ctx, insert, targetID, sentMessageID); err != nil {
			return fmt.Errorf("copy provider Sent envelope recipients: %w", err)
		}
	}
	return persistMessageMetadataFlag(boundQuerier{ctx: ctx, q: tx}, s.dialect, targetID, providerSentEnvelopeObservedKey)
}

func persistMessageMetadataFlag(q boundQuerier, dialect Dialect, messageID int64, key string) error {
	var metadata sql.NullString
	if err := q.QueryRow(`SELECT metadata FROM messages WHERE id=?`, messageID).Scan(&metadata); err != nil {
		return fmt.Errorf("read message metadata: %w", err)
	}
	values := map[string]jsontext.Value{}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &values); err != nil {
			return fmt.Errorf("decode message metadata: %w", err)
		}
	}
	if values == nil {
		values = make(map[string]jsontext.Value)
	}
	if observed, ok := values[key]; ok && string(observed) == "true" {
		return nil
	}
	values[key] = jsontext.Value("true")
	encoded, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode message metadata: %w", err)
	}
	if err := setMessageMetadataWith(q, dialect, messageID, sql.NullString{String: string(encoded), Valid: true}); err != nil {
		return fmt.Errorf("save message metadata flag %q: %w", key, err)
	}
	return nil
}
