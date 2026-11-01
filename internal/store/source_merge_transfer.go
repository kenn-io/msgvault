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
	if err := preserveMergedIMAPDrafts(ctx, tx, from, into); err != nil {
		return fmt.Errorf("preserve merged IMAP draft evidence: %w", err)
	}
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
	// Batch the ordinary row transfer and provenance writes. The values table
	// keeps provider IDs and hidden-survivor decisions in the same transaction.
	for start := 0; start < len(plan.messages); start += 100 {
		batch := plan.messages[start:min(start+100, len(plan.messages))]
		values := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*6)
		for _, m := range batch {
			var survivor any
			basis := ""
			if m.survivor != 0 {
				survivor = m.survivor
				basis = "payload-fingerprint"
				if m.kind.String == MessageTypeEmail && m.rfcID.String != "" {
					basis = "rfc822-or-payload"
				}
			}
			values = append(values, "(CAST(? AS BIGINT), CAST(? AS BIGINT), ?, ?, CAST(? AS BIGINT), ?)")
			args = append(args, m.id, conversations[m.conversationID], m.archivalID, m.providerID, survivor, basis)
		}
		incoming := `WITH incoming(id, conversation_id, archival_id, provider_id, survivor_id, match_basis) AS (VALUES ` + strings.Join(values, ",") + `) `
		if _, err := tx.ExecContext(ctx, incoming+`UPDATE messages SET source_id = ?,
 conversation_id = (SELECT conversation_id FROM incoming WHERE incoming.id = messages.id),
 source_message_id = (SELECT archival_id FROM incoming WHERE incoming.id = messages.id), embed_gen = NULL,
 deleted_at = CASE WHEN deleted_from_source_at IS NULL AND
 (SELECT survivor_id FROM incoming WHERE incoming.id = messages.id) IS NOT NULL
 THEN COALESCE(deleted_at, `+s.dialect.Now()+`) ELSE deleted_at END
 WHERE id IN (SELECT id FROM incoming)`, append(args, into)...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, incoming+`INSERT INTO source_merge_messages(message_id, original_source_id, original_message_id, survivor_message_id, archive_only, match_basis)
 SELECT id, ?, provider_id, survivor_id, TRUE, match_basis FROM incoming WHERE TRUE
 ON CONFLICT (message_id) DO UPDATE SET survivor_message_id = COALESCE(excluded.survivor_message_id, source_merge_messages.survivor_message_id),
 archive_only = TRUE, match_basis = CASE WHEN excluded.match_basis != '' THEN excluded.match_basis ELSE source_merge_messages.match_basis END`, append(args, from)...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, incoming+`INSERT INTO source_merge_archive_only_messages(message_id, original_source_message_id)
 SELECT id, COALESCE(provider_id, '') FROM incoming WHERE TRUE
 ON CONFLICT (message_id) DO UPDATE SET original_source_message_id =
 CASE WHEN source_merge_archive_only_messages.original_source_message_id = '' THEN excluded.original_source_message_id
 ELSE source_merge_archive_only_messages.original_source_message_id END`, args...); err != nil {
			return err
		}
	}
	// Gmail's canonical IDs also carry trusted roles. Preserve that evidence
	// before archive labels lose provider IDs; Drafts takes precedence.
	if _, err := tx.ExecContext(ctx, `UPDATE labels SET system_role = CASE
 WHEN system_role = 'drafts' OR source_label_id = 'DRAFT' THEN 'drafts'
 ELSE 'sent' END
 WHERE source_id IN (?, ?) AND source_label_id IN ('SENT', 'DRAFT')
 AND EXISTS (SELECT 1 FROM sources s WHERE s.id = labels.source_id AND s.source_type = 'gmail')`, from, into); err != nil {
		return err
	}
	// Incoming labels are archive organization, not destination provider label IDs.
	rows, err := tx.QueryContext(ctx, `SELECT l.id, l.name, COALESCE(l.system_role, ''), EXISTS(SELECT 1 FROM source_merge_preserved_labels p WHERE p.label_id = l.id) FROM labels l WHERE l.source_id = ? ORDER BY l.id`, from)
	if err != nil {
		return err
	}
	type label struct {
		id         int64
		name, role string
		preserved  bool
	}
	var labels []label
	for rows.Next() {
		var l label
		if err := rows.Scan(&l.id, &l.name, &l.role, &l.preserved); err != nil {
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
		var targetRole string
		err := tx.QueryRowContext(ctx, `SELECT id, COALESCE(system_role, '') FROM labels WHERE source_id = ? AND name = ?`, into, l.name).Scan(&target, &targetRole)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (targetRole != l.role || l.preserved) {
			// Sharing a name does not prove the same provider role. Keep the
			// incoming role without changing the destination label's meaning.
			// Previously archived roles also stay separate across later merges.
			target = 0
			l.name = fmt.Sprintf("msgvault-archive:%d:label:%d", from, l.id)
			for {
				var used bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM labels WHERE source_id = ? AND name = ?)`, into, l.name).Scan(&used); err != nil {
					return err
				}
				if !used {
					break
				}
				l.name += ":archive"
			}
		}
		if target == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE labels SET source_id = ?, name = ?, source_label_id = NULL WHERE id = ?`, into, l.name, l.id); err != nil {
				return err
			}
			if l.role != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_preserved_labels(label_id) VALUES (?) ON CONFLICT (label_id) DO NOTHING`, l.id); err != nil {
					return err
				}
			}
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
	if err := refreshSourceMessageAttributionContext(ctx, tx, into); err != nil {
		return err
	}
	// Moved mail uses a new mailbox, and incoming aliases or a Sent folder can
	// change attribution for existing destination mail too. Mark these rows in
	// the merge transaction; derive them in pages after it commits.
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET account_address = NULL, account_path = NULL
 WHERE source_id = ? AND COALESCE(message_type, '') IN ('', 'email', 'calendar_event')`, into); err != nil {
		return fmt.Errorf("mark merged account attribution pending: %w", err)
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

func preserveMergedIMAPDrafts(ctx context.Context, tx *loggedTx, from, into int64) error {
	const selection = `SELECT m.id FROM messages m JOIN sources src ON src.id = m.source_id
 WHERE m.source_id = ?
 AND EXISTS (SELECT 1 FROM imap_message_memberships imm
 WHERE imm.message_id = m.id AND imm.source_id = m.source_id
 AND LOWER(CAST(imm.flags AS TEXT)) LIKE '%"\\draft"%' ESCAPE '!')
 AND NOT EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
 WHERE ml.message_id = m.id AND l.source_id = m.source_id
 AND (l.system_role = 'drafts' OR (src.source_type = 'gmail' AND l.source_label_id = 'DRAFT')))`
	var needed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (`+selection+`)`, from).Scan(&needed); err != nil {
		return err
	}
	if !needed {
		return nil
	}
	name := fmt.Sprintf("msgvault-archive:%d:imap-draft", from)
	for {
		var used bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM labels WHERE source_id = ? AND name = ?)`, into, name).Scan(&used); err != nil {
			return err
		}
		if !used {
			break
		}
		name += ":archive"
	}
	var labelID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO labels(source_id, name, system_role)
 VALUES (?, ?, 'drafts') RETURNING id`, into, name).Scan(&labelID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_preserved_labels(label_id) VALUES (?)`, labelID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO message_labels(message_id, label_id) SELECT id, ? FROM (`+selection+`) drafts`, labelID, from)
	return err
}

// Copy only occurrences not already represented on the survivor. Compare
// archived file identity and presentation, not source-local MIME/provider IDs.
// Counting matching occurrences preserves repeated files within one message.
const missingMergedAttachmentPredicate = `(
 SELECT COUNT(*) FROM attachments b WHERE b.message_id = ?
 AND b.content_hash = a.content_hash
 AND COALESCE(b.filename, '') = COALESCE(a.filename, '')
 AND COALESCE(b.mime_type, '') = COALESCE(a.mime_type, '')
 AND COALESCE(b.size, 0) = COALESCE(a.size, 0)
 AND COALESCE(b.attachment_role, '') = COALESCE(a.attachment_role, '')
 AND COALESCE(b.content_id, '') = COALESCE(a.content_id, '')
) < (
 SELECT COUNT(*) FROM attachments h WHERE h.message_id = a.message_id AND h.id <= a.id
 AND h.content_hash = a.content_hash
 AND COALESCE(h.filename, '') = COALESCE(a.filename, '')
 AND COALESCE(h.mime_type, '') = COALESCE(a.mime_type, '')
 AND COALESCE(h.size, 0) = COALESCE(a.size, 0)
 AND COALESCE(h.attachment_role, '') = COALESCE(a.attachment_role, '')
 AND COALESCE(h.content_id, '') = COALESCE(a.content_id, '')
) OR COALESCE(a.content_hash, '') = ''`

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

	rows, err := tx.QueryContext(ctx, `SELECT a.id FROM attachments a WHERE a.message_id = ? AND (`+missingMergedAttachmentPredicate+`) ORDER BY a.id`, from, into)
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
