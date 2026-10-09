package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) applySourceMergePlan(ctx context.Context, tx *loggedTx, from, into int64, plan sourceMergePlan) error {
	conversations := map[int64]int64{}
	for _, c := range plan.conversations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_conversations(conversation_id, original_source_id, original_conversation_id, destination_conversation_id) VALUES (?, ?, ?, ?) ON CONFLICT (conversation_id) DO UPDATE SET destination_conversation_id = excluded.destination_conversation_id`, c.id, from, c.providerID, c.destination); err != nil {
			return err
		}
		conversations[c.id] = c.destination
		if c.destination == c.id {
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET source_id = ?, source_conversation_id = ? WHERE id = ?`, into, c.archivalID, c.id); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_participants(conversation_id, participant_id, role, joined_at, left_at)
 SELECT ?, participant_id, role, joined_at, left_at FROM conversation_participants WHERE conversation_id = ?
 ON CONFLICT (conversation_id, participant_id) DO NOTHING`, c.destination, c.id); err != nil {
				return err
			}
		}
	}
	for _, m := range plan.messages {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET source_id = ?, conversation_id = ?, source_message_id = ?, embed_gen = NULL WHERE id = ?`, into, conversations[m.conversationID], m.archivalID, m.id); err != nil {
			return err
		}
		basis := ""
		if m.survivor != 0 {
			basis = "payload-fingerprint"
			if m.kind.String == MessageTypeEmail && m.rfcID.String != "" {
				basis = "rfc822-or-payload"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET deleted_at = `+s.dialect.Now()+` WHERE id = ? AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, m.id); err != nil {
				return err
			}
		}
		var survivor any
		if m.survivor != 0 {
			survivor = m.survivor
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_messages(message_id, original_source_id, original_message_id, survivor_message_id, archive_only, match_basis)
 VALUES (?, ?, ?, ?, TRUE, ?) ON CONFLICT (message_id) DO UPDATE SET survivor_message_id = COALESCE(excluded.survivor_message_id, source_merge_messages.survivor_message_id),
 archive_only = TRUE, match_basis = CASE WHEN excluded.match_basis != '' THEN excluded.match_basis ELSE source_merge_messages.match_basis END`, m.id, from, m.providerID, survivor, basis); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_archive_only_messages(message_id, original_source_message_id)
 VALUES (?, ?) ON CONFLICT (message_id) DO UPDATE SET original_source_message_id =
 CASE WHEN source_merge_archive_only_messages.original_source_message_id = '' THEN excluded.original_source_message_id
      ELSE source_merge_archive_only_messages.original_source_message_id END`, m.id, m.providerID.String); err != nil {
			return err
		}
	}
	// Incoming labels are archive organization, not destination provider label IDs.
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM labels WHERE source_id = ? ORDER BY id`, from)
	if err != nil {
		return err
	}
	type label struct {
		id   int64
		name string
	}
	var labels []label
	for rows.Next() {
		var l label
		if err := rows.Scan(&l.id, &l.name); err != nil {
			_ = rows.Close()
			return err
		}
		labels = append(labels, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, l := range labels {
		var target int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM labels WHERE source_id = ? AND name = ?`, into, l.name).Scan(&target)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `UPDATE labels SET source_id = ?, source_label_id = NULL WHERE id = ?`, into, l.id); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO message_labels(message_id, label_id) SELECT message_id, ? FROM message_labels WHERE label_id = ? ON CONFLICT (message_id, label_id) DO NOTHING`, target, l.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE label_id = ?`, l.id); err != nil {
				return err
			}
			// Retain the old definition for historical source inspection.
		}
	}
	for _, m := range plan.messages {
		if m.survivor == 0 {
			continue
		}
		if err := copyMergedMessageEvidence(ctx, tx, m.id, m.survivor, from); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to_message_id = ? WHERE reply_to_message_id = ?`, m.survivor, m.id); err != nil {
			return err
		}
	}
	if err := s.mergeSourceIdentityEvidence(ctx, tx, from, into); err != nil {
		return err
	}
	if err := refreshSourceMessageAttributionContext(ctx, tx, into, ""); err != nil {
		return err
	}
	// Source-local receipts, sync runs, cursors, re-anchor markers, import receipts
	// and Beeper media endpoint records remain on the retired source for audit.
	for _, statement := range []string{
		`INSERT INTO collection_sources(collection_id, source_id) SELECT collection_id, ? FROM collection_sources WHERE source_id = ? ON CONFLICT (collection_id, source_id) DO NOTHING`,
		`UPDATE document_occurrences SET source_id = ? WHERE source_id = ?`,
		`UPDATE activity_events SET source_id = ? WHERE source_id = ?`,
		`UPDATE activity_events SET owner_source_id = ? WHERE owner_source_id = ?`,
		`UPDATE person_contact_state SET last_contact_source_id = ? WHERE last_contact_source_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, into, from); err != nil {
			return err
		}
	}
	// Observation rows and accepted-match endpoints retain their immutable IDs.
	// A duplicate current observation becomes historical rather than being deleted.
	if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations SET superseded_at = `+s.dialect.Now()+`
 WHERE source_id = ? AND active_until IS NULL AND superseded_at IS NULL
 AND EXISTS (SELECT 1 FROM participant_contact_observations b WHERE b.source_id = ?
 AND b.participant_id = participant_contact_observations.participant_id
 AND b.address_kind = participant_contact_observations.address_kind
 AND (b.service_id = participant_contact_observations.service_id OR
      (b.service_id IS NULL AND participant_contact_observations.service_id IS NULL))
 AND (b.scope_kind = participant_contact_observations.scope_kind OR
      (b.scope_kind IS NULL AND participant_contact_observations.scope_kind IS NULL))
 AND (b.scope_value = participant_contact_observations.scope_value OR
      (b.scope_value IS NULL AND participant_contact_observations.scope_value IS NULL))
 AND b.normalized_value = participant_contact_observations.normalized_value AND b.active_until IS NULL AND b.superseded_at IS NULL)`, from, into); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations SET source_id = ? WHERE source_id = ?`, into, from); err != nil {
		return err
	}
	for _, relation := range []struct{ table, key string }{{"identity_match_candidate_sources", "candidate_id"}, {"identity_match_evidence_sources", "evidence_id"}} {
		statement := fmt.Sprintf(`INSERT INTO %s(%s, source_id, is_conservative) SELECT %s, ?, is_conservative FROM %s WHERE source_id = ?
 ON CONFLICT (%s, source_id) DO UPDATE SET is_conservative = %s.is_conservative OR excluded.is_conservative`, relation.table, relation.key, relation.key, relation.table, relation.key, relation.table)
		if _, err := tx.ExecContext(ctx, statement, into, from); err != nil {
			return err
		}
	}
	if err := s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
		return fmt.Errorf("reconcile merged contact observation identity matches: %w", err)
	}
	// Queue triggers cover changed source/conversation/deletion inputs. Counts
	// and previews must ignore the retained hidden payloads immediately.
	for _, c := range plan.conversations {
		for _, id := range []int64{c.id, c.destination} {
			if err := s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = ?", id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET message_count = (SELECT COUNT(*) FROM messages WHERE conversation_id = conversations.id AND deleted_at IS NULL AND deleted_from_source_at IS NULL) WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE document_index_state SET revision = revision + 1, updated_at = `+s.dialect.Now()+` WHERE singleton = 1`); err != nil {
		return err
	}
	if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	if err := s.bumpAccountIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	return s.bumpDerivedDataRevision(tx)
}

func copyMergedMessageEvidence(ctx context.Context, tx *loggedTx, from, into, originalSource int64) error {
	for _, statement := range []string{
		`INSERT INTO message_labels(message_id, label_id) SELECT ?, label_id FROM message_labels WHERE message_id = ? ON CONFLICT (message_id, label_id) DO NOTHING`,
		`INSERT INTO message_recipients(message_id, participant_id, recipient_type, display_name, email_address) SELECT ?, participant_id, recipient_type, display_name, email_address FROM message_recipients WHERE message_id = ? ON CONFLICT DO NOTHING`,
		`INSERT INTO reactions(message_id, participant_id, reaction_type, reaction_value, created_at, removed_at) SELECT ?, participant_id, reaction_type, reaction_value, created_at, removed_at FROM reactions WHERE message_id = ? ON CONFLICT (message_id, participant_id, reaction_type, reaction_value) DO NOTHING`,
	} {
		if _, err := tx.ExecContext(ctx, statement, into, from); err != nil {
			return err
		}
	}
	const attachmentColumns = `filename, mime_type, size, content_hash, storage_path, media_type, width, height, duration_ms,
 thumbnail_hash, thumbnail_path, source_attachment_id, attachment_metadata, attachment_state, attachment_skip_reason,
 attachment_role, role_source, source_part_key, content_id, encryption_version, created_at`

	rows, err := tx.QueryContext(ctx, `SELECT id FROM attachments WHERE message_id = ? ORDER BY id`, from)
	if err != nil {
		return err
	}
	var attachmentIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		attachmentIDs = append(attachmentIDs, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range attachmentIDs {
		key := fmt.Sprintf("msgvault-archive:%d:attachment:%d", originalSource, id)
		selectColumns := strings.Replace(attachmentColumns, "source_part_key,", "CASE WHEN source_part_key IS NOT NULL THEN ? ELSE NULL END,", 1)
		var copied int64
		err := tx.QueryRowContext(ctx, `INSERT INTO attachments(message_id, `+attachmentColumns+`) SELECT ?, `+selectColumns+` FROM attachments WHERE id = ? ON CONFLICT DO NOTHING RETURNING id`, into, key, id).Scan(&copied)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_attachments(attachment_id, original_attachment_id, original_source_id) VALUES (?, ?, ?)`, copied, id, originalSource); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_preserved_attachments(attachment_id) VALUES (?) ON CONFLICT (attachment_id) DO NOTHING`, copied); err != nil {
			return err
		}
		// Only the copied occurrence loses provider eligibility. Original live
		// destination attachments retain their pending state and endpoint.
		if _, err := tx.ExecContext(ctx, `UPDATE attachments SET attachment_state = 'unavailable', attachment_skip_reason = 'historical-source' WHERE id = ? AND COALESCE(content_hash, '') = ''`, copied); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx, `UPDATE messages SET embed_gen = NULL, attachment_count = (SELECT COUNT(*) FROM attachments WHERE message_id = messages.id),
 has_attachments = EXISTS (SELECT 1 FROM attachments WHERE message_id = messages.id) WHERE id = ?`, into)
	return err
}

func (s *Store) mergeSourceIdentityEvidence(ctx context.Context, tx *loggedTx, from, into int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT address, source_signal, confirmed_at FROM account_identities WHERE source_id = ? ORDER BY address`, from)
	if err != nil {
		return err
	}
	type identity struct {
		address, signals string
		confirmed        time.Time
	}
	// Scan a native timestamp to retain its precision on both backends.
	var identities []identity
	for rows.Next() {
		var i identity
		if err := rows.Scan(&i.address, &i.signals, &i.confirmed); err != nil {
			_ = rows.Close()
			return err
		}
		identities = append(identities, i)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, i := range identities {
		if _, err := s.mergeAccountIdentitySignalsTx(ctx, tx, into, i.address, strings.Split(i.signals, ","), newIdentifierMatch(i.address)); err != nil {
			return err
		}
		confirmed := s.dialect.TimestampParam(i.confirmed)
		if _, err := tx.ExecContext(ctx, `UPDATE account_identities SET confirmed_at = ? WHERE source_id = ? AND address_key = ? AND confirmed_at > ?`, confirmed, into, NormalizeIdentifierForCompare(i.address), confirmed); err != nil {
			return err
		}
	}
	return nil
}
