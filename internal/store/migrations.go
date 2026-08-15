package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
)

// One-time data migrations run by InitSchema and gated on the
// applied_migrations ledger. Without the gate their no-op verification
// re-runs on every daemon start and scales with archive size (the
// last_modified backfill alone is a full messages-table scan — seconds of
// startup on a large archive).
const (
	migrationAttachmentsContentHashUnique = "attachments_content_hash_unique_index"
	migrationMessagesLastModifiedBackfill = "messages_last_modified_backfill"
	// v3: messageIdentityAttributionMatch became envelope-authoritative and
	// gated email identifier matches on the sender lacking a primary email.
	// Archives that ran v2 reconciled under the old predicate, so the rename
	// re-runs reconciliation (and the cache-revision bump) once more.
	migrationMessageAttributionProvenance     = "message_attribution_provenance_v3"
	migrationArchiveIdentity                  = "archive_identity_v1"
	migrationMessagesContentChangedAtBackfill = "messages_content_changed_at_backfill"
	migrationMessageWatermarkTriggers         = "message_watermark_triggers_v1"
	migrationEmbeddingChangeJournalTriggers   = "embedding_change_journal_triggers_v7"
	migrationIdentityMatchSourceSupport       = "identity_match_source_support_v1"
	// v3: the SQLite conversation trigger narrowed from a blanket
	// AFTER UPDATE to conversation_type changes only; archives that
	// installed the blanket trigger need the repair to re-run.
	// v4: the messages trigger narrowed on both backends to
	// MessagesActivityColumns with a value-change guard, so embedding and
	// FTS bookkeeping sweeps no longer requeue the archive.
	migrationActivityProjectionTriggers = "activity_projection_triggers_v4"
)

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
