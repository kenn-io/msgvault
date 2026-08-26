package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

const maxPersonSweepChangeScan = 1_000

// LatestPersonSweepChangeSequence returns the last sequence allocated by a
// committed person-linked archive mutation.
func (s *Store) LatestPersonSweepChangeSequence(ctx context.Context) (int64, error) {
	var sequence int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE
	`).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("read person sweep change sequence: %w", err)
	}
	return sequence, nil
}

// ScanPersonSweepChanges returns one tracked person's journal rows after a
// durable sequence cursor, strictly in commit order.
func (s *Store) ScanPersonSweepChanges(
	ctx context.Context, personID int64, after int64, limit int,
) ([]peoplesweep.ArchiveChange, error) {
	if limit <= 0 {
		return []peoplesweep.ArchiveChange{}, nil
	}
	if limit > maxPersonSweepChangeScan {
		limit = maxPersonSweepChangeScan
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT sequence, person_id, source_lane, change_kind, evidence_effect,
		       COALESCE(source_id, 0), COALESCE(message_id, 0),
		       COALESCE(attachment_id, 0), occurrence_key, recorded_at
		FROM person_sweep_changes
		WHERE person_id = ? AND sequence > ?
		ORDER BY sequence
		LIMIT ?`), personID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("scan person sweep changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	changes := make([]peoplesweep.ArchiveChange, 0, limit)
	for rows.Next() {
		var (
			change     peoplesweep.ArchiveChange
			recordedAt requiredTimestamp
		)
		if err := rows.Scan(
			&change.Sequence, &change.PersonID, &change.SourceLane,
			&change.Kind, &change.EvidenceEffect, &change.SourceID,
			&change.MessageID, &change.AttachmentID, &change.OccurrenceKey,
			&recordedAt,
		); err != nil {
			return nil, fmt.Errorf("scan person sweep change: %w", err)
		}
		change.RecordedAt = recordedAt.Time.UTC()
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person sweep changes: %w", err)
	}
	return changes, nil
}

// CoalescePersonSweepChangesContext reports how many durable people are dirty
// in the immutable journal. A source selects its exact source coordinate; nil
// covers the whole journal. Task 4 consumes the same grouping to upsert one
// work row per person at MAX(sequence).
func (s *Store) CoalescePersonSweepChangesContext(
	ctx context.Context, sourceID *int64,
) (int64, error) {
	query := `SELECT COUNT(DISTINCT person_id) FROM person_sweep_changes`
	args := []any(nil)
	if sourceID != nil {
		query += ` WHERE source_id = ?`
		args = append(args, *sourceID)
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, s.Rebind(query), args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("coalesce person sweep changes: %w", err)
	}
	return count, nil
}

// coalescePersonSweepChangesTx publishes one sync's bounded journal interval
// into one work row per currently tracked person. The clock lock fixes the
// completion high water: later journal commits receive a larger sequence and
// remain gap-detector debt rather than leaking into this publication.
func (s *Store) coalescePersonSweepChangesTx(
	ctx context.Context, tx *loggedTx, syncRunID, sourceID int64,
) error {
	// Archives upgraded while a sync was already running have no start cut.
	// Capture completion as their lower bound so old journal history is not
	// replayed; Task 10 recovers any skipped mutations as cursor gaps.
	if _, err := tx.ExecContext(ctx, s.Rebind(`
		INSERT INTO person_sweep_sync_publications
			(sync_run_id, source_id, lower_sequence)
		SELECT ?, ?, sequence
		FROM person_sweep_change_clock
		WHERE singleton = TRUE
		ON CONFLICT (sync_run_id) DO NOTHING`), syncRunID, sourceID); err != nil {
		return fmt.Errorf("ensure sync person sweep lower bound: %w", err)
	}
	var lowerBound int64
	if err := tx.QueryRowContext(ctx, s.Rebind(`
		SELECT lower_sequence
		FROM person_sweep_sync_publications
		WHERE sync_run_id = ? AND source_id = ?`), syncRunID, sourceID).Scan(&lowerBound); err != nil {
		return fmt.Errorf("read sync person sweep lower bound: %w", err)
	}
	var completionHighWater int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE person_sweep_change_clock
		SET sequence = sequence
		WHERE singleton = TRUE
		RETURNING sequence`).Scan(&completionHighWater); err != nil {
		return fmt.Errorf("capture sync person sweep completion high water: %w", err)
	}
	query := `
		SELECT c.person_id, MAX(c.sequence)
		FROM person_sweep_changes c
		JOIN person_tracking pt ON pt.person_id = c.person_id
		WHERE c.source_id = ? AND c.sequence > ? AND c.sequence <= ?
		GROUP BY c.person_id
		ORDER BY c.person_id`
	rows, err := tx.QueryContext(ctx, s.Rebind(query), sourceID, lowerBound,
		completionHighWater)
	if err != nil {
		return fmt.Errorf("scan person sweep changes for publication: %w", err)
	}
	type dirtyPerson struct {
		personID int64
		sequence int64
	}
	dirty := make([]dirtyPerson, 0)
	for rows.Next() {
		var person dirtyPerson
		if err := rows.Scan(&person.personID, &person.sequence); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan dirty person sweep high water: %w", err)
		}
		dirty = append(dirty, person)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate dirty person sweep high water: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close dirty person sweep high water: %w", err)
	}
	for _, person := range dirty {
		if err := s.upsertPersonSweepWorkTx(
			ctx, tx, person.personID, person.sequence); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE person_sweep_sync_publications
		SET upper_sequence = ?, published_at = %s
		WHERE sync_run_id = ? AND source_id = ?`, s.dialect.Now())),
		completionHighWater, syncRunID, sourceID); err != nil {
		return fmt.Errorf("publish sync person sweep upper bound: %w", err)
	}
	return nil
}

func (s *Store) coalescePersonSweepPeopleTx(
	ctx context.Context, tx *loggedTx, personIDs []int64,
) error {
	personIDs = slices.Clone(personIDs)
	slices.Sort(personIDs)
	personIDs = slices.Compact(personIDs)
	for _, personID := range personIDs {
		var sequence int64
		err := tx.QueryRowContext(ctx, s.Rebind(`
			SELECT COALESCE(MAX(c.sequence), 0)
			FROM person_sweep_changes c
			WHERE c.person_id = ?
			  AND EXISTS (SELECT 1 FROM person_tracking pt WHERE pt.person_id = c.person_id)`),
			personID).Scan(&sequence)
		if err != nil {
			return fmt.Errorf("read person %d sweep high water: %w", personID, err)
		}
		if sequence == 0 {
			continue
		}
		if err := s.upsertPersonSweepWorkTx(ctx, tx, personID, sequence); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appendPersonSweepChangeTx(
	ctx context.Context, tx *loggedTx, change peoplesweep.ArchiveChange,
) error {
	var sequence int64
	err := tx.QueryRowContext(ctx, `
		UPDATE person_sweep_change_clock
		SET sequence = sequence + 1
		WHERE singleton = TRUE AND enabled = TRUE
		RETURNING sequence`).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("allocate person sweep change sequence: %w", err)
	}
	_, err = tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		INSERT INTO person_sweep_changes
			(sequence, person_id, source_lane, change_kind, evidence_effect,
			 source_id, message_id, attachment_id, occurrence_key, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, %s)`, s.dialect.Now())),
		sequence, change.PersonID, change.SourceLane, change.Kind,
		change.EvidenceEffect, nullIfZero(change.SourceID),
		nullIfZero(change.MessageID), nullIfZero(change.AttachmentID),
		change.OccurrenceKey)
	if err != nil {
		return fmt.Errorf("append person sweep change: %w", err)
	}
	return nil
}

// appendPersonSweepMessageInsert is the SQLite production-path equivalent of
// the PostgreSQL message INSERT trigger. SQLite deliberately has no row
// trigger on this hot path because even an inert trigger forces a statement
// journal for every message insert.
func appendPersonSweepMessageInsert(q querier, d Dialect, messageID int64) error {
	recipientRole := personSweepRecipientRolePredicate("mr.recipient_type")
	roster := personSweepRosterPredicateSQL("pp.person_id")
	scope := fmt.Sprintf(`
		SELECT pp.person_id,
		       CASE WHEN m.message_type = 'meeting_transcript'
		            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
		       m.source_id, m.id AS message_id
		FROM messages m
		JOIN person_participants pp ON pp.participant_id = m.sender_id
		JOIN person_tracking pt ON pt.person_id = pp.person_id
		WHERE m.id = ? AND %s
		UNION
		SELECT pp.person_id,
		       CASE WHEN m.message_type = 'meeting_transcript'
		            THEN 'meeting_text' ELSE 'conversation_text' END,
		       m.source_id, m.id
		FROM messages m
		JOIN message_recipients mr ON mr.message_id = m.id
		JOIN person_participants pp ON pp.participant_id = mr.participant_id
		JOIN person_tracking pt ON pt.person_id = pp.person_id
		WHERE m.id = ? AND %s AND %s
		UNION
		SELECT pp.person_id,
		       CASE WHEN m.message_type = 'meeting_transcript'
		            THEN 'meeting_text' ELSE 'conversation_text' END,
		       m.source_id, m.id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN conversation_participants cp ON cp.conversation_id = c.id
		JOIN person_participants pp ON pp.participant_id = cp.participant_id
		JOIN person_tracking pt ON pt.person_id = pp.person_id
		WHERE m.id = ? AND %s AND %s`,
		LiveMessagesWhere("m", true), recipientRole, LiveMessagesWhere("m", true),
		roster, LiveMessagesWhere("m", true))
	messageArgs := []any{messageID, messageID, messageID}
	if _, err := q.Exec(`UPDATE person_sweep_change_clock
		SET sequence = sequence + (SELECT COUNT(*) FROM (`+scope+`) affected)
		WHERE singleton = TRUE AND enabled = TRUE`, messageArgs...); err != nil {
		return fmt.Errorf("allocate inserted message sweep changes: %w", err)
	}
	insertArgs := append(append([]any{}, messageArgs...), messageArgs...)
	if _, err := q.Exec(fmt.Sprintf(`INSERT INTO person_sweep_changes
			(sequence, person_id, source_lane, change_kind, evidence_effect,
			 source_id, message_id, recorded_at)
		SELECT clock.sequence - totals.total +
		           ROW_NUMBER() OVER (ORDER BY affected.person_id,
		                                        affected.source_id,
		                                        affected.message_id),
		       affected.person_id, affected.source_lane, 'upsert', '',
		       affected.source_id, affected.message_id, %s
		FROM (`+scope+`) affected
		CROSS JOIN (SELECT COUNT(*) AS total FROM (`+scope+`)) totals
		JOIN person_sweep_change_clock clock ON clock.singleton = TRUE
		WHERE clock.enabled = TRUE`, d.Now()), insertArgs...); err != nil {
		return fmt.Errorf("append inserted message sweep changes: %w", err)
	}
	return nil
}

func (s *Store) trackedPeopleForMessageTx(
	ctx context.Context, tx *loggedTx, messageID int64,
) ([]int64, error) {
	recipientRole := personSweepRecipientRolePredicate("mr.recipient_type")
	roster := personSweepRosterPredicateSQL("pp.person_id")
	rows, err := tx.QueryContext(ctx, s.Rebind(fmt.Sprintf(`
		SELECT pp.person_id
		FROM messages m
		JOIN person_participants pp ON pp.participant_id = m.sender_id
		JOIN person_tracking pt ON pt.person_id = pp.person_id
		WHERE m.id = ? AND %s
		UNION
		SELECT pp.person_id
		FROM messages m
		JOIN message_recipients mr ON mr.message_id = m.id
		JOIN person_participants pp ON pp.participant_id = mr.participant_id
		JOIN person_tracking pt ON pt.person_id = pp.person_id
		WHERE m.id = ? AND %s AND %s
		UNION
		SELECT pp.person_id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN conversation_participants cp ON cp.conversation_id = c.id
		JOIN person_participants pp ON pp.participant_id = cp.participant_id
		JOIN person_tracking pt ON pt.person_id = pp.person_id
		WHERE m.id = ? AND %s AND %s
		ORDER BY 1`, LiveMessagesWhere("m", true), recipientRole,
		LiveMessagesWhere("m", true), roster, LiveMessagesWhere("m", true))),
		messageID, messageID, messageID)
	if err != nil {
		return nil, fmt.Errorf("read tracked people for message %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	people := make([]int64, 0)
	for rows.Next() {
		var personID int64
		if err := rows.Scan(&personID); err != nil {
			return nil, fmt.Errorf("scan tracked person for message %d: %w", messageID, err)
		}
		people = append(people, personID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracked people for message %d: %w", messageID, err)
	}
	return people, nil
}

func (s *Store) publishPersonIdentityScopeChangesTx(
	ctx context.Context,
	tx *loggedTx,
	personIDs []int64,
	effect peoplesweep.EvidenceChangeEffect,
) error {
	personIDs = slices.Clone(personIDs)
	slices.Sort(personIDs)
	personIDs = slices.Compact(personIDs)
	trackedPersonIDs := make([]int64, 0, len(personIDs))
	for _, personID := range personIDs {
		var tracked bool
		if err := tx.QueryRowContext(ctx, s.Rebind(`
			SELECT EXISTS (
				SELECT 1 FROM person_tracking WHERE person_id = ?
			)`), personID).Scan(&tracked); err != nil {
			return fmt.Errorf("check person %d tracking for identity sweep: %w", personID, err)
		}
		if tracked {
			trackedPersonIDs = append(trackedPersonIDs, personID)
		}
	}
	personIDs = trackedPersonIDs
	for _, personID := range personIDs {
		rows, err := tx.QueryContext(ctx, s.Rebind(fmt.Sprintf(`
			WITH scoped_messages AS (
				SELECT m.source_id, m.id, m.message_type
				FROM messages m
				WHERE %s AND (
				EXISTS (SELECT 1 FROM person_participants pp
				        WHERE pp.person_id = ? AND pp.participant_id = m.sender_id)
				OR EXISTS (SELECT 1 FROM message_recipients mr
				           JOIN person_participants pp ON pp.participant_id = mr.participant_id
				           WHERE mr.message_id = m.id AND pp.person_id = ? AND %s)
				OR EXISTS (SELECT 1 FROM conversations c
				           JOIN conversation_participants cp ON cp.conversation_id = c.id
				           JOIN person_participants pp ON pp.participant_id = cp.participant_id
				           WHERE c.id = m.conversation_id AND pp.person_id = ? AND %s)
				)
			)
			SELECT scoped.source_id, scoped.id,
			       CASE WHEN scoped.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       0 AS attachment_id, '' AS occurrence_key
			FROM scoped_messages scoped
			UNION ALL
			SELECT scoped.source_id, scoped.id, 'document_text',
			       occurrence.attachment_id, occurrence.occurrence_key
			FROM scoped_messages scoped
			JOIN document_occurrences occurrence ON occurrence.message_id = scoped.id
			ORDER BY 2, 4, 5`, LiveMessagesWhere("m", true),
			personSweepRecipientRolePredicate("mr.recipient_type"),
			personSweepRosterPredicateSQL("pp.person_id"))),
			personID, personID, personID)
		if err != nil {
			return fmt.Errorf("read person %d identity scope: %w", personID, err)
		}
		changes := make([]peoplesweep.ArchiveChange, 0)
		for rows.Next() {
			change := peoplesweep.ArchiveChange{
				PersonID: personID, Kind: peoplesweep.ChangeScope,
				EvidenceEffect: effect,
			}
			if err := rows.Scan(&change.SourceID, &change.MessageID, &change.SourceLane,
				&change.AttachmentID, &change.OccurrenceKey); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan person %d identity scope: %w", personID, err)
			}
			changes = append(changes, change)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate person %d identity scope: %w", personID, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close person %d identity scope: %w", personID, err)
		}
		for _, change := range changes {
			if err := s.appendPersonSweepChangeTx(ctx, tx, change); err != nil {
				return err
			}
		}
	}
	return s.coalescePersonSweepPeopleTx(ctx, tx, personIDs)
}

func (s *Store) trackedPersonIDsForParticipantsTx(
	ctx context.Context, tx *loggedTx, participantIDs []int64,
) ([]int64, error) {
	personSet := make(map[int64]struct{})
	for _, participantID := range participantIDs {
		rows, err := tx.QueryContext(ctx, s.Rebind(`
			SELECT pt.person_id
			FROM person_tracking pt
			WHERE EXISTS (
				SELECT 1 FROM person_participants pp
				WHERE pp.person_id = pt.person_id AND pp.participant_id = ?
			)
			ORDER BY pt.person_id`), participantID)
		if err != nil {
			return nil, fmt.Errorf("read tracked people for participant %d: %w", participantID, err)
		}
		for rows.Next() {
			var personID int64
			if err := rows.Scan(&personID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan tracked person for participant %d: %w", participantID, err)
			}
			personSet[personID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate tracked people for participant %d: %w", participantID, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close tracked people for participant %d: %w", participantID, err)
		}
	}
	people := make([]int64, 0, len(personSet))
	for personID := range personSet {
		people = append(people, personID)
	}
	slices.Sort(people)
	return people, nil
}

func (s *Store) publishDocumentPersonSweepChangesTx(
	ctx context.Context,
	tx *loggedTx,
	canonicalBlobHash string,
	effect peoplesweep.EvidenceChangeEffect,
) error {
	rows, err := tx.QueryContext(ctx, s.Rebind(`
		SELECT o.occurrence_key, o.attachment_id, o.message_id, m.source_id
		FROM document_occurrences o
		JOIN attachments a ON a.id = o.attachment_id
		JOIN messages m ON m.id = o.message_id
		WHERE o.canonical_blob_hash = ?
		  AND o.attachment_role = 'standalone'
		  AND `+authoritativeDocumentRoleSourceSQL("o")+`
		  AND a.attachment_role = 'standalone'
		  AND `+authoritativeDocumentRoleSourceSQL("a")+`
		  AND (COALESCE(a.content_hash, '') = ? OR
		       (COALESCE(a.content_hash, '') = '' AND a.storage_path = ?))
		  AND `+LiveMessagesWhere("m", true)+`
		ORDER BY o.occurrence_key`), canonicalBlobHash, canonicalBlobHash,
		canonicalCASPath(canonicalBlobHash))
	if err != nil {
		return fmt.Errorf("read document sweep occurrences: %w", err)
	}
	type documentOccurrence struct {
		occurrenceKey string
		attachmentID  int64
		messageID     int64
		sourceID      int64
	}
	occurrences := make([]documentOccurrence, 0)
	for rows.Next() {
		var occurrence documentOccurrence
		if err := rows.Scan(&occurrence.occurrenceKey, &occurrence.attachmentID,
			&occurrence.messageID, &occurrence.sourceID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan document sweep occurrence: %w", err)
		}
		occurrences = append(occurrences, occurrence)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate document sweep occurrences: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close document sweep occurrences: %w", err)
	}

	personSet := make(map[int64]struct{})
	for _, occurrence := range occurrences {
		people, err := s.trackedPeopleForMessageTx(ctx, tx, occurrence.messageID)
		if err != nil {
			return err
		}
		for _, personID := range people {
			personSet[personID] = struct{}{}
			if err := s.appendPersonSweepChangeTx(ctx, tx, peoplesweep.ArchiveChange{
				PersonID: personID, SourceLane: peoplesweep.SourceDocumentText,
				Kind: peoplesweep.ChangePublication, EvidenceEffect: effect,
				SourceID: occurrence.sourceID, MessageID: occurrence.messageID,
				AttachmentID:  occurrence.attachmentID,
				OccurrenceKey: occurrence.occurrenceKey,
			}); err != nil {
				return err
			}
		}
	}
	personIDs := make([]int64, 0, len(personSet))
	for personID := range personSet {
		personIDs = append(personIDs, personID)
	}
	return s.coalescePersonSweepPeopleTx(ctx, tx, personIDs)
}

func (s *Store) publishLinkedDocumentOccurrencePersonSweepChangesTx(
	ctx context.Context,
	tx *loggedTx,
	occurrence DocumentOccurrence,
) error {
	var hasEvidence bool
	err := tx.QueryRowContext(ctx, s.Rebind(`
		SELECT EXISTS (
			SELECT 1
			FROM document_extraction_heads h
			JOIN document_extraction_profiles p ON p.id = h.profile_id
			JOIN document_provider_consents consent ON consent.profile_id = p.id
			JOIN document_chunks dc ON dc.extraction_id = h.extraction_id
			JOIN document_occurrences o ON o.canonical_blob_hash = h.canonical_blob_hash
			JOIN attachments a ON a.id = o.attachment_id
			JOIN messages m ON m.id = o.message_id
			CROSS JOIN document_index_state ds
			WHERE o.occurrence_key = ? AND `+documentSearchValidityForConsent("consent")+`
		)`), occurrence.OccurrenceKey).Scan(&hasEvidence)
	if err != nil {
		return fmt.Errorf("check linked document occurrence sweep evidence: %w", err)
	}
	if !hasEvidence {
		return nil
	}

	personIDs, err := s.trackedPeopleForMessageTx(ctx, tx, occurrence.MessageID)
	if err != nil {
		return err
	}
	for _, personID := range personIDs {
		if err := s.appendPersonSweepChangeTx(ctx, tx, peoplesweep.ArchiveChange{
			PersonID: personID, SourceLane: peoplesweep.SourceDocumentText,
			Kind:           peoplesweep.ChangePublication,
			EvidenceEffect: peoplesweep.EvidenceEffectScopeRelinked,
			SourceID:       occurrence.SourceID, MessageID: occurrence.MessageID,
			AttachmentID: occurrence.AttachmentID, OccurrenceKey: occurrence.OccurrenceKey,
		}); err != nil {
			return err
		}
	}
	return s.coalescePersonSweepPeopleTx(ctx, tx, personIDs)
}

func (s *Store) publishDocumentOccurrencePersonSweepChangeTx(
	ctx context.Context,
	tx *loggedTx,
	occurrence DocumentOccurrence,
	kind peoplesweep.ChangeKind,
	effect peoplesweep.EvidenceChangeEffect,
) error {
	personIDs, err := s.trackedPeopleForMessageTx(ctx, tx, occurrence.MessageID)
	if err != nil {
		return err
	}
	for _, personID := range personIDs {
		if err := s.appendPersonSweepChangeTx(ctx, tx, peoplesweep.ArchiveChange{
			PersonID: personID, SourceLane: peoplesweep.SourceDocumentText,
			Kind: kind, EvidenceEffect: effect,
			SourceID: occurrence.SourceID, MessageID: occurrence.MessageID,
			AttachmentID: occurrence.AttachmentID, OccurrenceKey: occurrence.OccurrenceKey,
		}); err != nil {
			return err
		}
	}
	return s.coalescePersonSweepPeopleTx(ctx, tx, personIDs)
}

// These fragments mirror personscope's default from/to/group union. Journal
// triggers use literal row aliases instead of bind arguments, so keeping the
// shared semantics here prevents the SQLite and PostgreSQL definitions from
// growing separate interpretations of archive membership.
func personSweepRecipientRolePredicate(recipientType string) string {
	return fmt.Sprintf("LOWER(%s) IN ('from', 'to', 'cc', 'bcc')", recipientType)
}

func personSweepRecipientRoleChangeKindSQL(oldType, newType string) string {
	oldAuthoritative := fmt.Sprintf("COALESCE(%s, FALSE)",
		personSweepRecipientRolePredicate(oldType))
	newAuthoritative := fmt.Sprintf("COALESCE(%s, FALSE)",
		personSweepRecipientRolePredicate(newType))
	return fmt.Sprintf(`CASE WHEN (%s) = (%s) THEN 'upsert' ELSE 'scope' END`,
		oldAuthoritative, newAuthoritative)
}

func personSweepRecipientRoleChangeEffectSQL(oldType, newType string) string {
	oldAuthoritative := fmt.Sprintf("COALESCE(%s, FALSE)",
		personSweepRecipientRolePredicate(oldType))
	newAuthoritative := fmt.Sprintf("COALESCE(%s, FALSE)",
		personSweepRecipientRolePredicate(newType))
	return fmt.Sprintf(`CASE WHEN NOT (%s) AND (%s) THEN 'scope-relinked'
		WHEN (%s) AND NOT (%s) THEN 'scope-unlinked'
		ELSE 'source-edited' END`, oldAuthoritative, newAuthoritative,
		oldAuthoritative, newAuthoritative)
}

func personSweepDirectionEvidenceValuesSQL(messageID, isFromMe, senderID string) string {
	return fmt.Sprintf(`(%s = TRUE OR %s IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = %s
		  AND LOWER(known_from.recipient_type) = 'from'))`, isFromMe, senderID, messageID)
}

func personSweepRosterPredicateSQL(personID string) string {
	return personSweepRosterPredicateValuesSQL(
		"m.id", "m.is_from_me", "m.sender_id",
		"c.conversation_type", personID,
	)
}

func personSweepRosterPredicateValuesSQL(
	messageID, isFromMe, senderID, conversationType, personID string,
) string {
	directionEvidence := personSweepDirectionEvidenceValuesSQL(messageID, isFromMe, senderID)
	return fmt.Sprintf(`(
		(LOWER(COALESCE(%s, '')) = 'direct_chat'
		 AND %s
		 AND NOT EXISTS (
			 SELECT 1 FROM person_participants sender_person
			 WHERE sender_person.person_id = %s
			   AND (sender_person.participant_id = %s OR (
				 %s IS NULL AND EXISTS (
					 SELECT 1 FROM message_recipients scoped_sender
					 WHERE scoped_sender.message_id = %s
					   AND LOWER(scoped_sender.recipient_type) = 'from'
					   AND scoped_sender.participant_id = sender_person.participant_id)))))
		OR (LOWER(COALESCE(%s, '')) <> 'direct_chat' OR NOT %s))`,
		conversationType, directionEvidence, personID, senderID,
		senderID, messageID, conversationType, directionEvidence)
}
