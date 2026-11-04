package store

import (
	"context"
	"fmt"
)

func (s *Store) backfillMicrosoftSentEnvelopeRecipients(ctx context.Context, tx *loggedTx) error {
	sentObserved := `COALESCE(CASE WHEN json_valid(m.metadata)
		THEN json_extract(m.metadata, '$.msmail_sent_items_observed') END=1, FALSE)`
	if s.IsPostgreSQL() {
		sentObserved = `COALESCE(m.metadata ->> 'msmail_sent_items_observed'='true', FALSE)`
	}
	original := `COALESCE(NULLIF((SELECT p.original_source_message_id
		FROM source_merge_archive_only_messages p WHERE p.message_id=m.id), ''), m.source_message_id)`
	email := `COALESCE(NULLIF(m.message_type,''),'email')='email'`
	rawImported := `(` + email + ` AND ` + s.senderRawImportedIDSQL(original) + `)`
	sentLabel := `EXISTS (
			SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
			WHERE ml.message_id=m.id AND l.source_id=m.source_id AND l.system_role=?)`
	query := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO msmail_sent_envelope_recipients
		(message_id, email_address)
		SELECT m.id, LOWER(COALESCE(TRIM(mr.email_address), ''))
		FROM messages m
		JOIN sources src ON src.id=m.source_id
		JOIN message_recipients mr ON mr.message_id=m.id
		WHERE src.source_type='msmail'
		  AND mr.recipient_type IN ('to','cc','bcc')
		  AND LOWER(COALESCE(TRIM(mr.email_address), ''))<>''
		  AND (` + sentObserved + ` OR (` + sentLabel + ` AND NOT ` + rawImported + `))`)
	if _, err := tx.ExecContext(ctx, query, LabelSystemRoleSent); err != nil {
		return fmt.Errorf("copy existing Microsoft Sent envelopes: %w", err)
	}
	return nil
}
