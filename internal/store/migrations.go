package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// One-time data migrations run by InitSchema and gated on the
// applied_migrations ledger. Without the gate their no-op verification
// re-runs on every daemon start and scales with archive size (the
// last_modified backfill alone is a full messages-table scan — seconds of
// startup on a large archive).
const (
	migrationAttachmentsContentHashUnique = "attachments_content_hash_unique_index"
	migrationAttachmentOccurrenceUnique   = "attachment_occurrence_unique_indexes_v1"
	migrationMessagesLastModifiedBackfill = "messages_last_modified_backfill"
	// v3: messageIdentityAttributionMatch became envelope-authoritative and
	// gated email identifier matches on the sender lacking a primary email.
	// Archives that ran v2 reconciled under the old predicate, so the rename
	// re-runs reconciliation (and the cache-revision bump) once more.
	migrationMessageAttributionProvenance     = "message_attribution_provenance_v3"
	migrationArchiveIdentity                  = "archive_identity_v1"
	migrationMessagesContentChangedAtBackfill = "messages_content_changed_at_backfill"
	// v4: the visual invalidation triggers record a cleared pending vector
	// token in the visual_obsolete_tokens ledger, clear durable outcomes on
	// message context changes, and cover message_bodies deletion.
	// v5: hard-deleting a publication row (message or source cascade)
	// ledgers its current and pending tokens first, so backend vectors of
	// hard-deleted messages stay sweepable.
	// v6: a message_type change seeds stale placeholder publications for
	// the message's standalone attachments, so a message entering a
	// type-scoped visual lane is discovered by the sweep without an
	// attachment journal event or a full rebuild; out-of-scope placeholders
	// are tombstoned by the same sweep.
	// v7: the scope-entry seed only accepts canonical 64-hex hashes or
	// strict CAS paths; malformed owners would wedge the tombstone sweep.
	// v8: message_bodies deletion bumps content_changed_at so a deleted
	// body cannot pass the visual content-stamp CAS.
	// v9: the scope-entry seed revives a tombstoned publication back to
	// stale (a scope exit tombstones it; DO NOTHING left re-entry
	// unindexable until a full rebuild).
	migrationMessageWatermarkTriggers       = "message_and_attachment_triggers_v9"
	migrationEmbeddingChangeJournalTriggers = "embedding_change_journal_triggers_v7"
	// v2: message updates share the content-column/value guard, participant
	// scope mirrors personscope, and metadata-only edge edits are not identity
	// reassignments.
	// v3: recipient role edits that cross the authoritative-role boundary are
	// scope relinks/unlinks; edits within the boundary remain source edits.
	// v4: source deletion/reimport, scope unlink/relink, and identity
	// reassignment publish lane-exact document_text changes with the serving
	// occurrence's attachment and occurrence coordinates. Archives that ran v3
	// must reinstall both backend trigger sets so document evidence lifecycle
	// changes can match the canonical document lane instead of only the owning
	// conversation_text or meeting_text row.
	migrationPersonSweepChangeTriggers   = "person_sweep_change_triggers_v5"
	migrationIdentityMatchSourceSupport  = "identity_match_source_support_v1"
	migrationVCardSourceResourceIdentity = "vcard_source_resource_identity_v1"
	migrationOrganizationDomainIDNA      = "organization_domain_idna_v1"
	migrationGmailChatClassification     = "gmail_chat_classification_v1"
	// v3: the SQLite conversation trigger narrowed from a blanket
	// AFTER UPDATE to conversation_type changes only; archives that
	// installed the blanket trigger need the repair to re-run.
	// v4: the messages trigger narrowed on both backends to
	// MessagesActivityColumns with a value-change guard, so embedding and
	// FTS bookkeeping sweeps no longer requeue the archive.
	migrationActivityProjectionTriggers = "activity_projection_triggers_v4"
	migrationPersonInferenceProviderV2  = "person_inference_provider_v2"
	migrationPersonSweepCallsV2         = "person_sweep_calls_v2"
)

func (s *Store) classifyLegacyGmailChats(ctx context.Context, tx *loggedTx) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET conversation_type = 'chat', updated_at = `+s.dialect.Now()+`
		WHERE conversation_type = 'email_thread'
		  AND id IN (
			SELECT m.conversation_id
			FROM labels label
			JOIN sources source ON source.id = label.source_id
			JOIN message_labels ml ON ml.label_id = label.id
			JOIN messages m ON m.id = ml.message_id AND m.source_id = source.id
			WHERE source.source_type = 'gmail'
			  AND label.source_label_id = 'CHAT'
			  AND m.message_type = 'email'
		  )
	`); err != nil {
		return fmt.Errorf("classify legacy Gmail chat conversations: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET message_type = ?
		WHERE message_type = 'email'
		  AND id IN (
			SELECT ml.message_id
			FROM labels label
			JOIN sources source ON source.id = label.source_id
			JOIN message_labels ml ON ml.label_id = label.id
			JOIN messages candidate ON candidate.id = ml.message_id
				AND candidate.source_id = source.id
			WHERE source.source_type = 'gmail'
			  AND label.source_label_id = 'CHAT'
		  )
	`, MessageTypeGoogleChat); err != nil {
		return fmt.Errorf("classify legacy Gmail chat messages: %w", err)
	}
	return nil
}

type legacyOrganizationDomainIdentifier struct {
	id, organizationID int64
	stored, canonical  string
	active             bool
}

func (s *Store) canonicalizeLegacyOrganizationDomains(
	ctx context.Context, tx *loggedTx,
) error {
	affectedOrganizations := make(map[int64]struct{})
	rows, err := tx.QueryContext(ctx, `
		SELECT id, primary_domain
		FROM organizations
		WHERE primary_domain IS NOT NULL
		ORDER BY id
	`)
	if err != nil {
		return fmt.Errorf("list legacy organization primary domains: %w", err)
	}
	type primaryDomainUpdate struct {
		id        int64
		canonical string
	}
	var primaryUpdates []primaryDomainUpdate
	for rows.Next() {
		var id int64
		var stored string
		if err := rows.Scan(&id, &stored); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy organization primary domain: %w", err)
		}
		canonical := NormalizeDomain(stored)
		if canonical != "" && canonical != stored {
			primaryUpdates = append(primaryUpdates, primaryDomainUpdate{id, canonical})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate legacy organization primary domains: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy organization primary domains: %w", err)
	}

	for _, update := range primaryUpdates {
		if _, err := tx.ExecContext(ctx, `
			UPDATE organizations
			SET primary_domain = ?
			WHERE id = ?
		`, update.canonical, update.id); err != nil {
			return fmt.Errorf("canonicalize organization %d primary domain: %w", update.id, err)
		}
		affectedOrganizations[update.id] = struct{}{}
	}

	rows, err = tx.QueryContext(ctx, `
		SELECT id, organization_id, normalized_value,
		       active_until IS NULL AND superseded_at IS NULL AS active
		FROM organization_identifiers
		WHERE identifier_kind = 'domain'
		ORDER BY organization_id, id
	`)
	if err != nil {
		return fmt.Errorf("list legacy organization domain identifiers: %w", err)
	}
	var identifiers []legacyOrganizationDomainIdentifier
	for rows.Next() {
		var identifier legacyOrganizationDomainIdentifier
		if err := rows.Scan(
			&identifier.id, &identifier.organizationID, &identifier.stored,
			&identifier.active,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy organization domain identifier: %w", err)
		}
		identifier.canonical = NormalizeDomain(identifier.stored)
		if identifier.canonical == "" {
			continue
		}
		identifiers = append(identifiers, identifier)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate legacy organization domain identifiers: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy organization domain identifiers: %w", err)
	}

	type collisionKey struct {
		organizationID int64
		canonical      string
	}
	winners := make(map[collisionKey]int)
	var losers []int
	for index, identifier := range identifiers {
		if !identifier.active {
			continue
		}
		key := collisionKey{identifier.organizationID, identifier.canonical}
		winnerIndex, found := winners[key]
		if !found {
			winners[key] = index
			continue
		}
		winner := identifiers[winnerIndex]
		if identifier.stored == identifier.canonical && winner.stored != winner.canonical {
			losers = append(losers, winnerIndex)
			winners[key] = index
			continue
		}
		losers = append(losers, index)
	}

	now := time.Now().UTC()
	for _, index := range losers {
		identifier := identifiers[index]
		if _, err := tx.ExecContext(ctx, `
			UPDATE organization_identifiers
			SET active_until = CASE
			      WHEN active_from IS NULL OR active_from <= ? THEN ?
			      ELSE active_until
			    END,
			    superseded_at = ?,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ?
			  AND active_until IS NULL AND superseded_at IS NULL
		`, now, now, now, identifier.id); err != nil {
			return fmt.Errorf("retire colliding organization domain identifier %d: %w",
				identifier.id, err)
		}
		affectedOrganizations[identifier.organizationID] = struct{}{}
	}

	for _, identifier := range identifiers {
		if identifier.canonical == identifier.stored {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE organization_identifiers
			SET normalized_value = ?
			WHERE id = ?
		`, identifier.canonical, identifier.id); err != nil {
			return fmt.Errorf("canonicalize organization domain identifier %d: %w",
				identifier.id, err)
		}
		if identifier.active {
			affectedOrganizations[identifier.organizationID] = struct{}{}
		}
	}
	organizationIDs := make([]int64, 0, len(affectedOrganizations))
	for organizationID := range affectedOrganizations {
		organizationIDs = append(organizationIDs, organizationID)
	}
	organizationPlaceholders, args := sortedIDPlaceholders(organizationIDs)
	if organizationPlaceholders == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE organizations
		SET revision = revision + 1, updated_at = `+s.dialect.Now()+`
		WHERE id IN (`+organizationPlaceholders+`)
	`, args...); err != nil {
		return fmt.Errorf("bump canonicalized organization revisions: %w", err)
	}
	if err := s.bumpEmployedPersonVCardProjectionsTx(ctx, tx, organizationIDs...); err != nil {
		return err
	}
	return nil
}

// backfillLegacyIdentityMatchSourceSupport gives pre-support-table generated
// candidates and evidence a conservative source marker. Their exact source is
// not recoverable from the old rows, so each row that has no support records is
// attached to every source that exists during the upgrade. The marker keeps
// those associations from becoming subset-export dependencies. This prevents
// the removal of one unrelated source from deleting legacy review state while
// keeping unknown provenance out of shared archives. New rows always record
// their exact support through the normal writers.
func (s *Store) backfillLegacyIdentityMatchSourceSupport(
	ctx context.Context, tx *loggedTx,
) error {
	if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`
		INSERT OR IGNORE INTO identity_match_candidate_sources
			(candidate_id, source_id, is_conservative)
		SELECT candidate.id, source.id, TRUE
		FROM (
			SELECT candidate.id
			FROM identity_match_candidates candidate
			LEFT JOIN identity_match_candidate_sources support
			  ON support.candidate_id = candidate.id
			WHERE candidate.source IN ('archive_observation', 'extraction', 'enrichment')
			GROUP BY candidate.id
			HAVING COUNT(support.source_id) = 0
		) candidate
		CROSS JOIN sources source
	`)); err != nil {
		return fmt.Errorf("backfill legacy identity match candidate support: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`
		INSERT OR IGNORE INTO identity_match_evidence_sources
			(evidence_id, source_id, is_conservative)
		SELECT evidence.id, source.id, TRUE
		FROM (
			SELECT evidence.id
			FROM identity_match_evidence evidence
			LEFT JOIN identity_match_evidence_sources support
			  ON support.evidence_id = evidence.id
			WHERE evidence.source IN ('archive_observation', 'extraction', 'enrichment')
			GROUP BY evidence.id
			HAVING COUNT(support.source_id) = 0
		) evidence
		CROSS JOIN sources source
	`)); err != nil {
		return fmt.Errorf("backfill legacy identity match evidence support: %w", err)
	}
	return nil
}

func backfillLegacyMessageAttributionProvenance(
	ctx context.Context,
	tx *loggedTx,
) error {
	if err := backfillLegacyCalendarAttribution(ctx, tx); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET source_is_from_me = FALSE,
		    identity_is_from_me = COALESCE(is_from_me, FALSE)
		WHERE source_is_from_me IS NULL
		  AND source_id IN (
		    SELECT id
		    FROM sources
		    WHERE source_type IN ('granola', 'circleback')
		  )
	`)
	if err != nil {
		return fmt.Errorf("backfill message attribution provenance: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE messages
		SET source_is_from_me = COALESCE(is_from_me, FALSE),
		    identity_is_from_me = FALSE
		WHERE source_is_from_me IS NULL
	`)
	if err != nil {
		return fmt.Errorf("initialize source-native message attribution provenance: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM sources
		WHERE source_type IN ('granola', 'circleback')
		   OR EXISTS (
		     SELECT 1
		     FROM account_identities ai
		     WHERE ai.source_id = sources.id
		   )
	`)
	if err != nil {
		return fmt.Errorf("list identity-derived attribution sources: %w", err)
	}
	var sourceIDs []int64
	for rows.Next() {
		var sourceID int64
		if err := rows.Scan(&sourceID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan identity-derived attribution source: %w", err)
		}
		sourceIDs = append(sourceIDs, sourceID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate identity-derived attribution sources: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close identity-derived attribution sources: %w", err)
	}

	for _, sourceID := range sourceIDs {
		if err := refreshSourceMessageAttributionContext(ctx, tx, sourceID, ""); err != nil {
			return fmt.Errorf("reconcile source %d attribution: %w", sourceID, err)
		}
	}
	return nil
}

func backfillLegacyCalendarAttribution(
	ctx context.Context,
	tx *loggedTx,
) error {
	var lastMessageID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT m.id, mr.raw_data, mr.compression
			FROM messages m
			JOIN sources s ON s.id = m.source_id
			JOIN message_raw mr ON mr.message_id = m.id
			WHERE m.source_is_from_me IS NULL
			  AND COALESCE(m.is_from_me, FALSE) = TRUE
			  AND s.source_type = 'gcal'
			  AND mr.raw_format = 'gcal_json'
			  AND m.id > ?
			ORDER BY m.id
			LIMIT 500
		`, lastMessageID)
		if err != nil {
			return fmt.Errorf("list legacy calendar attribution: %w", err)
		}

		var (
			batchSize  int
			messageIDs []int64
		)
		for rows.Next() {
			var (
				messageID   int64
				rawData     []byte
				compression sql.NullString
			)
			if err := rows.Scan(&messageID, &rawData, &compression); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan legacy calendar attribution: %w", err)
			}
			lastMessageID = messageID
			batchSize++
			organizerSelf, ok := legacyCalendarOrganizerSelf(rawData, compression)
			if ok && !organizerSelf {
				messageIDs = append(messageIDs, messageID)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate legacy calendar attribution: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close legacy calendar attribution: %w", err)
		}

		if err := execInChunksContext(
			ctx,
			tx,
			messageIDs,
			nil,
			`UPDATE messages
			 SET source_is_from_me = FALSE,
			     identity_is_from_me = COALESCE(is_from_me, FALSE)
			 WHERE id IN (%s)`,
		); err != nil {
			return fmt.Errorf("backfill calendar attribution provenance: %w", err)
		}
		if batchSize < 500 {
			return nil
		}
	}
}

func legacyCalendarOrganizerSelf(
	rawData []byte,
	compression sql.NullString,
) (bool, bool) {
	if compression.Valid && compression.String == "zlib" {
		reader, err := zlib.NewReader(bytes.NewReader(rawData))
		if err != nil {
			return false, false
		}
		defer func() { _ = reader.Close() }()
		rawData, err = io.ReadAll(reader)
		if err != nil {
			return false, false
		}
	}

	var event struct {
		Organizer *struct {
			Self bool `json:"self"`
		} `json:"organizer"`
	}
	if err := json.Unmarshal(rawData, &event); err != nil || event.Organizer == nil {
		return false, false
	}
	return event.Organizer.Self, true
}

// IsMigrationApplied reports whether the named one-time data migration
// has already run.
func (s *Store) IsMigrationApplied(name string) (bool, error) {
	return s.IsMigrationAppliedContext(context.Background(), name)
}

// IsMigrationAppliedContext is the request-aware form of IsMigrationApplied.
func (s *Store) IsMigrationAppliedContext(ctx context.Context, name string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`, name,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check migration %q: %w", name, err)
	}
	return count > 0, nil
}

// MarkMigrationApplied records that a migration has run. Idempotent.
func (s *Store) MarkMigrationApplied(name string) error {
	return s.MarkMigrationAppliedContext(context.Background(), name)
}

// MarkMigrationAppliedContext is the request-aware form of
// MarkMigrationApplied.
func (s *Store) MarkMigrationAppliedContext(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx,
		s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO applied_migrations (name) VALUES (?)`),
		name,
	)
	if err != nil {
		return fmt.Errorf("mark migration %q applied: %w", name, err)
	}
	return nil
}
