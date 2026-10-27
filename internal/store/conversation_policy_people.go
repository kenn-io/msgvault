package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

// Eligibility is an independent, live dimension. A source reimport status may
// restore source support, but cannot restore permission to use a cold room.
func (s *Store) personFactConversationEligibleTx(ctx context.Context, tx *loggedTx, input personfacts.EvidenceInput) (bool, error) {
	if input.SourceClass != personfacts.EvidenceArchive {
		return true, nil
	}
	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(input.SourceRef)
	if err == nil {
		var eligible bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m WHERE m.id=? AND m.source_id=? AND `+FullMessageWhere("m")+`)`, ref.MessageID, ref.SourceID).Scan(&eligible)
		return eligible, err
	}
	// Unsupported archive evidence belongs to another aligner.
	return true, nil
}

// Promotion validates the exact source version and subject binding. No source
// deletion, edit, or identity withdrawal is synthesized as a policy event.
func (s *Store) personFactPolicyPromotionValidTx(ctx context.Context, tx *loggedTx, input personfacts.EvidenceInput) (bool, error) {
	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(input.SourceRef)
	switch err {
	case nil:
		// A recognized archive reference can be checked against live support.
	default:
		// Unsupported or foreign evidence cannot authorize promotion.
		return false, nil
	}
	var subject, sourceType, sourceMessageID, body, snippet string
	var bound bool
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(m.subject,''),COALESCE(m.snippet,''),src.source_type,COALESCE(m.source_message_id,''),
 EXISTS(SELECT 1 FROM person_participants pp WHERE pp.person_id=? AND (pp.participant_id=m.sender_id OR EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=pp.participant_id AND LOWER(mr.recipient_type)='from')))
 FROM messages m JOIN sources src ON src.id=m.source_id WHERE m.id=? AND m.source_id=? AND `+LiveMessagesWhere("m", true)+` AND `+FullMessageWhere("m"), input.PersonID, ref.MessageID, ref.SourceID).Scan(&subject, &snippet, &sourceType, &sourceMessageID, &bound)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bound || !personSweepSourceAuthenticatesSender(sourceType) || sourceMessageID != ref.SourceMessageID {
		return false, nil
	}
	if ref.SourceLane == peoplesweep.SourceDocumentText {
		var version string
		err = tx.QueryRowContext(ctx, `SELECT 'document/v1:' || h.extraction_id || ':' || dc.checksum
 FROM document_chunks dc JOIN document_extraction_heads h ON h.extraction_id=dc.extraction_id
 JOIN document_extraction_profiles p ON p.id=h.profile_id JOIN document_provider_consents consent ON consent.profile_id=p.id
 JOIN document_occurrences o ON o.canonical_blob_hash=h.canonical_blob_hash JOIN attachments a ON a.id=o.attachment_id
 JOIN messages m ON m.id=o.message_id CROSS JOIN document_index_state ds
 WHERE o.occurrence_key=? AND a.id=? AND m.id=? AND dc.chunk_key=? AND `+documentSearchValidityForConsent("consent"), ref.OccurrenceKey, ref.AttachmentID, ref.MessageID, ref.ChunkKey).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return version == input.SourceVersion, err
	}
	if ref.SourceLane != peoplesweep.SourceConversationText && ref.SourceLane != peoplesweep.SourceMeetingText {
		return false, nil
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(body_text,'') FROM message_bodies WHERE message_id=?`, ref.MessageID).Scan(&body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	body, _ = preprocess.Preprocess("", body, 0, preprocess.Config{StripQuotes: true, StripSignatures: true, StripHTML: true, StripBase64: true, CollapseWhitespace: true})
	return input.SourceVersion == "message/v1:"+sweepSHA256(canonicalSweepMessageText(subject, body, snippet)), nil
}

func (s *Store) invalidateConversationPolicyPeopleTx(ctx context.Context, tx *loggedTx, conversationIDs []int64, revision int64) error {
	people, err := s.conversationPolicyPeopleForRoomsTx(ctx, tx, conversationIDs)
	if err != nil {
		return err
	}
	return s.invalidateConversationPolicyPersonsTx(ctx, tx, people, revision)
}

func (s *Store) conversationPolicyPeopleForRoomsTx(ctx context.Context, tx *loggedTx, conversationIDs []int64) ([]int64, error) {
	// Bootstrap must include legacy observations even with no changed tier.
	legacyCondition := `EXISTS(SELECT 1 FROM participant_contact_observations o JOIN sources src ON src.id=o.source_id WHERE o.participant_id=pp.participant_id AND src.source_type IN ('beeper','slack','slackdump','discord','teams') AND o.source NOT IN ('user','carddav_import','vcard_import') AND NOT EXISTS(SELECT 1 FROM participant_contact_observation_conversations edge WHERE edge.observation_id=o.id))`
	peopleByID := make(map[int64]struct{})
	queryPeople := func(condition string, args []any) error {
		rows, err := tx.QueryContext(ctx, `SELECT p.id FROM persons p WHERE EXISTS(SELECT 1 FROM person_participants pp WHERE pp.person_id=p.id AND `+condition+`) ORDER BY p.id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			peopleByID[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	}
	if len(conversationIDs) == 0 {
		if err := queryPeople(legacyCondition, nil); err != nil {
			return nil, err
		}
	} else {
		for start := 0; start < len(conversationIDs); start += conversationPolicyRoomBatchSize {
			end := min(start+conversationPolicyRoomBatchSize, len(conversationIDs))
			marks, ids := sortedIDPlaceholders(conversationIDs[start:end])
			if len(ids) == 0 {
				continue
			}
			condition := `(EXISTS(SELECT 1 FROM conversation_participants cp WHERE cp.participant_id=pp.participant_id AND cp.conversation_id IN (` + marks + `)) OR EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id IN (` + marks + `) AND (m.sender_id=pp.participant_id OR EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=pp.participant_id))) OR EXISTS(SELECT 1 FROM participant_contact_observations o JOIN participant_contact_observation_conversations edge ON edge.observation_id=o.id WHERE o.participant_id=pp.participant_id AND edge.conversation_id IN (` + marks + `)))`
			args := make([]any, 0, len(ids)*3)
			args = append(args, ids...)
			args = append(args, ids...)
			args = append(args, ids...)
			if err := queryPeople(condition, args); err != nil {
				return nil, err
			}
		}
	}
	people := make([]int64, 0, len(peopleByID))
	for id := range peopleByID {
		people = append(people, id)
	}
	slices.Sort(people)
	return people, nil
}

func (s *Store) invalidateConversationPolicyPersonsTx(ctx context.Context, tx *loggedTx, people []int64, revision int64) error {
	var err error
	for _, person := range people {
		if err = s.reconcileConversationPolicyFactsTx(ctx, tx, person, revision); err != nil {
			return err
		}
	}
	if err = s.invalidateConversationPolicyPersonInputsTx(ctx, tx, people); err != nil {
		return err
	}
	// Existing scope changes replay old message/document coordinates below cursors.
	return s.publishPersonIdentityScopeChangesTx(ctx, tx, people, peoplesweep.EvidenceEffectNone)
}

func (s *Store) invalidateConversationPolicyPersonInputsTx(ctx context.Context, tx *loggedTx, people []int64) error {
	if len(people) == 0 {
		return nil
	}
	if err := s.bumpPersonRevisionsTx(ctx, tx, people...); err != nil {
		return err
	}
	if err := s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(ctx, tx, people...); err != nil {
		return err
	}
	marks, args := sortedIDPlaceholders(people)
	_, err := tx.ExecContext(ctx, `UPDATE person_briefs SET status='superseded',superseded_at=`+s.dialect.Now()+` WHERE status='current' AND person_id IN (`+marks+`)`, args...)
	return err
}

func (s *Store) reconcileConversationPolicyFactsTx(ctx context.Context, tx *loggedTx, person, revision int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+personFactEvidenceColumns+` FROM person_fact_evidence e WHERE e.person_id=? ORDER BY e.id`, person)
	if err != nil {
		return err
	}
	var evidence []personfacts.Evidence
	for rows.Next() {
		e, scanErr := scanPersonFactEvidence(rows)
		if scanErr != nil {
			err = scanErr
			break
		}
		evidence = append(evidence, e)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	if err = s.hydratePersonFactEvidenceStatusTx(ctx, tx, evidence); err != nil {
		return err
	}
	var changes []personfacts.EvidenceStatusChange
	var latestStatusID int64
	for _, e := range evidence {
		if e.LatestStatus != nil {
			latestStatusID = max(latestStatusID, e.LatestStatus.ID)
		}
		supported := e.LatestStatus == nil || e.LatestStatus.Supported
		reason := personfacts.EvidenceStatusConversationCold
		if !e.PolicyExcluded {
			if e.LatestStatus == nil || e.LatestStatus.Reason != personfacts.EvidenceStatusConversationCold {
				continue
			}
			valid, err := s.personFactPolicyPromotionValidTx(ctx, tx, e.Input)
			if err != nil {
				return err
			}
			if !valid {
				continue
			}
			reason = personfacts.EvidenceStatusConversationFull
		} else if !supported {
			continue
		}
		changes = append(changes, personfacts.EvidenceStatusChange{EvidenceKey: e.Key, SourceVersion: e.Input.SourceVersion, Supported: reason == personfacts.EvidenceStatusConversationFull, Reason: reason})
	}
	if len(changes) == 0 {
		return nil
	}
	cursor := strconv.FormatInt(revision, 10) + ":" + strconv.FormatInt(latestStatusID, 10)
	input := personfacts.GenerationInput{PersonID: person, SourceCursors: []personfacts.SourceCursor{{Lane: "conversation-policy", Start: cursor, End: cursor}}, ProgramID: "conversation-policy", ProgramVersion: "v1", ProgramFingerprint: sweepSHA256("conversation-policy/v1"), CatalogFingerprint: "current", Provider: "msgvault", ProviderVersion: "v1", ResolvedAt: time.Now().UTC(), Policy: personfacts.PolicyContext{AllowSensitive: true, ProviderPolicyFingerprint: "conversation-policy/v1"}, EvidenceStatusChanges: changes}
	prepared, err := personfacts.PreparePersonFactGeneration(ctx, input, nil)
	if err != nil {
		return fmt.Errorf("prepare policy fact changes: %w", err)
	}
	_, _, err = s.applyPreparedPersonFactGenerationTrackingTx(ctx, tx, prepared, nil, false)
	return err
}
