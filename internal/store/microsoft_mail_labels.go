package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const microsoftCategoryPrefix = "msmail-category:"

const microsoftSentEvidenceKey = "msmail_sent_items_observed"

// MicrosoftSentItemsObservedBatchContext reports messages whose Sent Items
// provenance is already recorded. Importers use this before moving a known
// message into Sent so they can refresh its MIME before capturing the first
// envelope snapshot.
func (s *Store) MicrosoftSentItemsObservedBatchContext(ctx context.Context, messageIDs []int64) (map[int64]bool, error) {
	observed := make(map[int64]bool, len(messageIDs))
	for _, id := range messageIDs {
		observed[id] = false
	}
	for start := 0; start < len(messageIDs); start += 500 {
		end := min(start+500, len(messageIDs))
		batch := messageIDs[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		microsoftSent := `COALESCE(CASE WHEN json_valid(m.metadata)
			THEN json_extract(m.metadata, '$.msmail_sent_items_observed') END=1, FALSE)`
		if s.IsPostgreSQL() {
			microsoftSent = `COALESCE(m.metadata ->> 'msmail_sent_items_observed'='true', FALSE)`
		}
		query := `SELECT m.id, CASE WHEN ` + microsoftSent + ` OR EXISTS (
			SELECT 1 FROM msmail_sent_envelope_recipients sent WHERE sent.message_id=m.id
		) OR EXISTS (
			SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
			WHERE ml.message_id=m.id AND l.source_id=m.source_id AND l.system_role=?
		) THEN 1 ELSE 0 END
		FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE src.source_type='msmail' AND m.id IN (` + placeholders + `)`
		args := make([]any, 0, len(batch)+1)
		args = append(args, LabelSystemRoleSent)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
		if err != nil {
			return nil, fmt.Errorf("read Microsoft Sent Items provenance: %w", err)
		}
		for rows.Next() {
			var id int64
			var hasEvidence int
			if err := rows.Scan(&id, &hasEvidence); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan Microsoft Sent Items provenance: %w", err)
			}
			observed[id] = hasEvidence != 0
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("iterate Microsoft Sent Items provenance: %w", err)
		}
	}
	return observed, nil
}

// EnsureMicrosoftMailFoldersContext updates folder labels. A category label
// holding a folder's display name moves to a distinct name so the two never merge.
func (s *Store) EnsureMicrosoftMailFoldersContext(ctx context.Context, sourceID int64, labels map[string]LabelInfo) (map[string]int64, error) {
	var result map[string]int64
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		folderNames := make(map[string]bool, len(labels))
		for providerID, info := range labels {
			if strings.HasPrefix(providerID, microsoftCategoryPrefix) {
				return errors.New("microsoft folder identity uses the category namespace")
			}
			folderNames[info.Name] = true
		}
		for _, info := range labels {
			var id int64
			var providerID sql.NullString
			err := q.QueryRow(`SELECT id,source_label_id FROM labels WHERE source_id=? AND name=?`, sourceID, info.Name).Scan(&id, &providerID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("check Microsoft folder name: %w", err)
			}
			if !strings.HasPrefix(providerID.String, microsoftCategoryPrefix) {
				continue
			}
			name, err := microsoftCategoryLabelName(q, sourceID, id, strings.TrimPrefix(providerID.String, microsoftCategoryPrefix), folderNames)
			if err != nil {
				return err
			}
			if _, err := q.Exec(`UPDATE labels SET name=? WHERE id=?`, name, id); err != nil {
				return fmt.Errorf("rename Microsoft category label: %w", err)
			}
		}
		var err error
		result, err = ensureLabelsBatchWith(q, sourceID, labels, labelFlipsTx(ctx, tx, sourceID))
		return err
	})
	return result, err
}

// microsoftCategoryLabelName returns "Category: <name>", or the first
// "Category: <name> (n)" that no other label of the source or reserved name uses.
func microsoftCategoryLabelName(q boundQuerier, sourceID, labelID int64, category string, reserved map[string]bool) (string, error) {
	base := "Category: " + category
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		if reserved[name] {
			continue
		}
		var owner int64
		err := q.QueryRow(`SELECT id FROM labels WHERE source_id=? AND name=?`, sourceID, name).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner == labelID) {
			return name, nil
		}
		if err != nil {
			return "", fmt.Errorf("check Microsoft category name: %w", err)
		}
	}
}

// ReconcileMicrosoftMailLabelsContext saves a folder and an optional category
// snapshot. Missing categories preserve the previous snapshot; an empty slice
// clears them. The result reports a folder change, not a category change.
func (s *Store) ReconcileMicrosoftMailLabelsContext(ctx context.Context, messageID int64, folderID *int64, categories *[]string) (bool, error) {
	var folderChanged bool
	err := s.withMessageAttributionTxContext(ctx, messageID, func(tx *loggedTx) error {
		var sourceID int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM messages
 WHERE id=? AND EXISTS (SELECT 1 FROM sources s WHERE s.id=messages.source_id AND s.source_type='msmail')`, messageID).Scan(&sourceID); err != nil {
			return fmt.Errorf("resolve Microsoft message labels: %w", err)
		}
		var err error
		folderChanged, err = s.reconcileMicrosoftMailLabelsTx(ctx, tx, sourceID, messageID, folderID, categories)
		return err
	})
	if err != nil {
		return false, err
	}
	return folderChanged, nil
}

func (s *Store) reconcileMicrosoftMailLabelsTx(ctx context.Context, tx *loggedTx, sourceID, messageID int64, folderID *int64, categories *[]string) (bool, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	sentObserved := false
	if folderID != nil {
		var source int64
		var providerID sql.NullString
		var systemRole sql.NullString
		if err := q.QueryRow(`SELECT source_id,source_label_id,system_role FROM labels WHERE id=?`, *folderID).Scan(&source, &providerID, &systemRole); err != nil {
			return false, fmt.Errorf("resolve Microsoft folder label: %w", err)
		}
		if source != sourceID || providerID.String == "" || strings.HasPrefix(providerID.String, microsoftCategoryPrefix) {
			return false, errors.New("microsoft folder label does not belong to the message source")
		}
		if systemRole.String == LabelSystemRoleSent {
			sentObserved = true
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.id,l.source_id,l.source_label_id,l.system_role
 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=?`, messageID)
	if err != nil {
		return false, fmt.Errorf("read Microsoft message labels: %w", err)
	}
	desired := make([]int64, 0)
	var folders []int64
	for rows.Next() {
		var id int64
		var source sql.NullInt64
		var providerID sql.NullString
		var systemRole sql.NullString
		if err := rows.Scan(&id, &source, &providerID, &systemRole); err != nil {
			_ = rows.Close()
			return false, err
		}
		category := source.Valid && source.Int64 == sourceID && strings.HasPrefix(providerID.String, microsoftCategoryPrefix)
		nativeFolder := source.Valid && source.Int64 == sourceID && providerID.String != "" && !category
		if nativeFolder {
			folders = append(folders, id)
			if systemRole.String == LabelSystemRoleSent {
				sentObserved = true
			}
		}
		if (category && categories != nil) || (nativeFolder && folderID != nil) {
			continue
		}
		desired = append(desired, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	folderChanged := false
	if folderID != nil {
		desired = append(desired, *folderID)
		folderChanged = len(folders) != 1 || folders[0] != *folderID
	}
	catalogChanged := false
	if categories != nil && len(*categories) > 0 {
		// Match the provider's Unicode case rules while keeping each label's ID stable.
		rows, err := tx.QueryContext(ctx, `SELECT source_label_id FROM labels WHERE source_id=? AND source_label_id LIKE ?`, sourceID, microsoftCategoryPrefix+"%")
		if err != nil {
			return false, fmt.Errorf("read Microsoft category identities: %w", err)
		}
		var categoryProviderIDs []string
		for rows.Next() {
			var providerID string
			if err := rows.Scan(&providerID); err != nil {
				_ = rows.Close()
				return false, err
			}
			categoryProviderIDs = append(categoryProviderIDs, providerID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, err
		}
		if err := rows.Close(); err != nil {
			return false, err
		}
		for _, category := range *categories {
			providerID := microsoftCategoryPrefix + category
			if i := slices.IndexFunc(categoryProviderIDs, func(existing string) bool {
				return strings.EqualFold(existing, providerID)
			}); i >= 0 {
				providerID = categoryProviderIDs[i]
			}
			var id int64
			var previousName string
			var kind sql.NullString
			err := q.QueryRow(`SELECT id,name,label_type FROM labels WHERE source_id=? AND source_label_id=?`, sourceID, providerID).Scan(&id, &previousName, &kind)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("read Microsoft category %q: %w", category, err)
			}
			name, nameErr := microsoftCategoryLabelName(q, sourceID, id, category, nil)
			if nameErr != nil {
				return false, nameErr
			}
			switch {
			case errors.Is(err, sql.ErrNoRows):
				err = q.QueryRow(`INSERT INTO labels (source_id,source_label_id,name,label_type) VALUES (?,?,?,'user') RETURNING id`, sourceID, providerID, name).Scan(&id)
				categoryProviderIDs = append(categoryProviderIDs, providerID)
				catalogChanged = true
			case err == nil && (previousName != name || kind.String != "user"):
				_, err = q.Exec(`UPDATE labels SET name=?,label_type='user',system_role=NULL WHERE id=?`, name, id)
				catalogChanged = true
			}
			if err != nil {
				return false, fmt.Errorf("save Microsoft category %q: %w", category, err)
			}
			if !slices.Contains(desired, id) {
				desired = append(desired, id)
			}
		}
	}
	if sentObserved {
		if err := persistMicrosoftSentEvidence(q, s.dialect, messageID); err != nil {
			return false, err
		}
	}
	changed, err := s.reconcileMessageLabelsTxContext(ctx, tx, messageID, desired, true)
	if err != nil {
		return false, err
	}
	if changed || catalogChanged {
		if err := s.bumpDerivedDataRevision(tx, true); err != nil {
			return false, err
		}
	}
	return folderChanged, nil
}

func persistMicrosoftSentEvidence(q boundQuerier, dialect Dialect, messageID int64) error {
	if err := persistMicrosoftSentEvidenceMetadata(q, dialect, messageID); err != nil {
		return err
	}
	if err := persistMicrosoftSentEnvelopeRecipients(q, dialect, messageID, messageID); err != nil {
		return fmt.Errorf("save Microsoft Sent Items envelope: %w", err)
	}
	return nil
}

func persistMicrosoftSentEnvelopeRecipients(q boundQuerier, dialect Dialect, targetID, sentMessageID int64) error {
	var sourceHasSnapshot bool
	if err := q.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM msmail_sent_envelope_recipients WHERE message_id=?
	)`, sentMessageID).Scan(&sourceHasSnapshot); err != nil {
		return fmt.Errorf("check Microsoft Sent Items snapshot: %w", err)
	}
	if sourceHasSnapshot {
		insert := dialect.InsertOrIgnore(`INSERT OR IGNORE INTO msmail_sent_envelope_recipients
			(message_id, email_address)
			SELECT ?, email_address
			FROM msmail_sent_envelope_recipients
			WHERE message_id=?`)
		if _, err := q.Exec(insert, targetID, sentMessageID); err != nil {
			return fmt.Errorf("copy Microsoft Sent Items snapshot: %w", err)
		}
		return nil
	}
	insert := dialect.InsertOrIgnore(`INSERT OR IGNORE INTO msmail_sent_envelope_recipients
		(message_id, email_address)
		SELECT ?, LOWER(COALESCE(TRIM(email_address), ''))
		FROM message_recipients
		WHERE message_id=? AND recipient_type IN ('to','cc','bcc')
		  AND LOWER(COALESCE(TRIM(email_address), ''))<>''`)
	if _, err := q.Exec(insert, targetID, sentMessageID); err != nil {
		return fmt.Errorf("copy Microsoft Sent Items recipients: %w", err)
	}
	return nil
}

func (s *Store) mergeMicrosoftSentEvidenceTx(ctx context.Context, tx *loggedTx, survivorID, duplicateID int64) error {
	sentObserved := func(message string) string {
		metadata := `CASE WHEN json_valid(` + message + `.metadata) THEN json_extract(` + message + `.metadata, '$.` + microsoftSentEvidenceKey + `') END=1`
		if s.IsPostgreSQL() {
			metadata = message + `.metadata ->> '` + microsoftSentEvidenceKey + `'='true'`
		}
		return `(` + metadata + ` OR EXISTS (
			SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
			WHERE ml.message_id=` + message + `.id AND l.source_id=` + message + `.source_id
			  AND l.system_role='` + LabelSystemRoleSent + `') OR EXISTS (
			SELECT 1 FROM msmail_sent_envelope_recipients sent
			WHERE sent.message_id=` + message + `.id))`
	}
	var observed, survivorObserved bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM messages duplicate
		JOIN messages survivor ON survivor.id=?
		JOIN sources src ON src.id=duplicate.source_id
		WHERE duplicate.id=? AND duplicate.source_id=survivor.source_id
		  AND src.source_type='msmail' AND `+sentObserved("duplicate")+`
	), EXISTS (
		SELECT 1 FROM messages duplicate
		JOIN messages survivor ON survivor.id=?
		JOIN sources src ON src.id=survivor.source_id
		WHERE duplicate.id=? AND duplicate.source_id=survivor.source_id
		  AND src.source_type='msmail' AND `+sentObserved("survivor")+`
	)`, survivorID, duplicateID, survivorID, duplicateID).
		Scan(&observed, &survivorObserved)
	if err != nil {
		return fmt.Errorf("check duplicate Microsoft Sent Items provenance: %w", err)
	}
	if !observed {
		return nil
	}
	q := boundQuerier{ctx: ctx, q: tx}
	if err := persistMicrosoftSentEvidenceMetadata(q, s.dialect, survivorID); err != nil {
		return fmt.Errorf("preserve duplicate Microsoft Sent Items provenance: %w", err)
	}
	if survivorObserved {
		if err := persistMicrosoftSentEnvelopeRecipients(q, s.dialect, survivorID, survivorID); err != nil {
			return fmt.Errorf("preserve survivor Microsoft Sent Items envelope: %w", err)
		}
	}
	if err := persistMicrosoftSentEnvelopeRecipients(q, s.dialect, survivorID, duplicateID); err != nil {
		return fmt.Errorf("preserve duplicate Microsoft Sent Items envelope: %w", err)
	}
	transferRecipientsSQL := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO message_recipients
		(message_id, participant_id, recipient_type, display_name, email_address)
		SELECT ?, participant_id, recipient_type, display_name, email_address
		FROM message_recipients
		WHERE message_id=? AND recipient_type IN ('to','cc','bcc')`)
	if _, err := tx.ExecContext(ctx, transferRecipientsSQL, survivorID, duplicateID); err != nil {
		return fmt.Errorf("preserve duplicate Microsoft Sent Items recipients: %w", err)
	}
	return nil
}

func persistMicrosoftSentEvidenceMetadata(q boundQuerier, dialect Dialect, messageID int64) error {
	var metadata sql.NullString
	if err := q.QueryRow(`SELECT metadata FROM messages WHERE id=?`, messageID).Scan(&metadata); err != nil {
		return fmt.Errorf("read Microsoft message metadata: %w", err)
	}
	values := map[string]jsontext.Value{}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &values); err != nil {
			return fmt.Errorf("decode Microsoft message metadata: %w", err)
		}
	}
	if values == nil {
		values = make(map[string]jsontext.Value)
	}
	if observed, ok := values[microsoftSentEvidenceKey]; ok && string(observed) == "true" {
		return nil
	}
	values[microsoftSentEvidenceKey] = jsontext.Value("true")
	encoded, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode Microsoft message metadata: %w", err)
	}
	if err := setMessageMetadataWith(q, dialect, messageID, sql.NullString{String: string(encoded), Valid: true}); err != nil {
		return fmt.Errorf("save Microsoft Sent Items provenance: %w", err)
	}
	return nil
}
