package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

// revalidateAppreciationMemoryTx runs after the source and sweep locks. A
// dismissal seen here wins over the in-flight model result; missing owner
// proof downgrades the surviving contribution to unknown.
func (s *Store) revalidateAppreciationMemoryTx(ctx context.Context, tx *loggedTx, personID int64, brief *peoplesweep.BriefResult, includeDismissed, validateOwner, lock bool) (map[string]peoplesweep.BriefAppreciationMemory, error) {
	var messageIDs []int64
	for _, memory := range brief.Boundary.AppreciationMemory {
		messageIDs = append(messageIDs, memory.PeerRef.MessageID)
		if memory.OwnerRef != nil {
			messageIDs = append(messageIDs, memory.OwnerRef.MessageID)
		}
	}
	slices.Sort(messageIDs)
	for _, id := range slices.Compact(messageIDs) {
		// Body upserts take this same message lock before reading or writing.
		if lock {
			if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
				return nil, fmt.Errorf("lock appreciation evidence: %w", err)
			}
		}
	}
	keys := make([]string, 0, len(brief.Boundary.AppreciationMemory))
	for _, memory := range brief.Boundary.AppreciationMemory {
		keys = append(keys, memory.Suggestion.Key)
	}
	states, err := getPersonAppreciationStates(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	current := make(map[string]peoplesweep.BriefAppreciationMemory, len(brief.Boundary.AppreciationMemory))
	kept := make([]peoplesweep.BriefAppreciationMemory, 0, len(brief.Boundary.AppreciationMemory))
	for _, memory := range brief.Boundary.AppreciationMemory {
		if !validAppreciationKey(memory.Suggestion.Key) {
			return nil, ErrInvalidPersonAppreciationKey
		}
		if _, duplicate := current[memory.Suggestion.Key]; duplicate {
			return nil, errors.New("duplicate appreciation contribution in brief")
		}
		if states[memory.Suggestion.Key].DismissedAt != nil && !includeDismissed {
			continue
		}
		peerTime, valid, err := s.appreciationPeerCurrentTx(ctx, tx, personID, memory, brief.ValidationProfile)
		if err != nil {
			return nil, err
		}
		if !valid {
			continue
		}
		ownerValid := false
		if validateOwner {
			ownerValid, err = s.appreciationOwnerCurrentTx(ctx, tx, personID, memory, brief.ValidationProfile, peerTime, lock)
			if err != nil {
				return nil, err
			}
		}
		if ownerValid {
			memory.Suggestion.State, memory.Suggestion.PriorThanks = "already_thanked", "evidenced"
		} else {
			memory.Suggestion.State, memory.Suggestion.PriorThanks = "suggested", "unknown"
			memory.Suggestion.Thanked, memory.OwnerRef, memory.OwnerRawSHA256 = nil, nil, ""
		}
		current[memory.Suggestion.Key] = memory
		kept = append(kept, memory)
	}
	brief.Boundary.AppreciationMemory = kept
	return current, nil
}

func recordAppreciationProofsTx(ctx context.Context, tx *loggedTx, briefID int64, memories []peoplesweep.BriefAppreciationMemory) error {
	for _, memory := range memories {
		if memory.Suggestion.State != "already_thanked" || memory.OwnerRef == nil || memory.Suggestion.Thanked == nil {
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_appreciation_state (contribution_key, thanks_brief_id)
 VALUES (?, ?) ON CONFLICT (contribution_key) DO UPDATE SET thanks_brief_id = excluded.thanks_brief_id`, memory.Suggestion.Key, briefID)
		if err != nil {
			return fmt.Errorf("record appreciation proof: %w", err)
		}
	}
	return nil
}

// appreciationPeerCurrentTx checks the exact native message again at the point
// at which a result is committed or projected. It deliberately fetches the
// body by primary key, separately from the metadata query.
func (s *Store) appreciationPeerCurrentTx(ctx context.Context, tx *loggedTx, personID int64, memory peoplesweep.BriefAppreciationMemory, profile peoplesweep.ProviderProfile) (time.Time, bool, error) {
	if !profile.AllowSensitive || !sweepSourceAllowed(profile.AllowedSources, peoplesweep.SourceConversationText) || memory.PeerRef.SourceLane != peoplesweep.SourceConversationText {
		return time.Time{}, false, nil
	}
	after, before, err := sweepDateBounds(profile.SourceSince, profile.SourceUntil)
	if err != nil {
		return time.Time{}, false, err
	}
	person, err := s.getPersonTx(ctx, tx, personID)
	if errors.Is(err, ErrPersonNotFound) || err == nil && len(person.ParticipantIDs) == 0 {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read appreciation scope: %w", err)
	}
	// Route the existing scope/provenance loader through the transaction that
	// holds the evidence locks, never through a second database connection.
	readCtx := context.WithValue(ctx, readSnapshotKey{}, &readSnapshot{db: s.db.DB, tx: tx})
	items, _, err := s.hydratePersonSweepMessageSet(readCtx, personID, peoplesweep.SourceConversationText, []int64{memory.PeerRef.MessageID}, 0)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("rehydrate appreciation peer: %w", err)
	}
	if len(items) != 1 || items[0].Ref != memory.PeerRef || items[0].SubjectPersonID == nil ||
		*items[0].SubjectPersonID != personID || items[0].Directness != personfacts.DirectSelf ||
		after != nil && items[0].EventTime.Before(*after) || before != nil && !items[0].EventTime.Before(*before) {
		return time.Time{}, false, nil
	}
	var conversationID int64
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id = ?`, memory.PeerRef.MessageID).Scan(&conversationID); errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	} else if err != nil {
		return time.Time{}, false, fmt.Errorf("read appreciation conversation: %w", err)
	}
	if conversationID != memory.ConversationID {
		return time.Time{}, false, nil
	}
	key, err := appreciationMessageKeyTx(ctx, tx, memory.PeerRef.MessageID)
	if err != nil || key == "" || key != memory.Suggestion.Key {
		return time.Time{}, false, err
	}
	var body sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`, memory.PeerRef.MessageID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read appreciation peer body: %w", err)
	}
	return items[0].EventTime.UTC(), body.Valid && memory.Suggestion.Quote != "" &&
		sweepSHA256(body.String) == memory.PeerRawSHA256 && strings.Contains(body.String, memory.Suggestion.Quote), nil
}

// appreciationDirectOwnerTx resolves a provider-complete direct roster. Writers
// lock its conversation and participant rows; snapshot readers do not.
func (s *Store) appreciationDirectOwnerTx(ctx context.Context, tx *loggedTx, personID, conversationID int64, lock bool) (int64, int64, bool, error) {
	if lock && s.dialect.DriverName() == postgresDriverName {
		// The parent lock excludes new roster rows via their foreign key;
		// locking the existing rows below also excludes removal or replacement.
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE id = ? FOR UPDATE`, conversationID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, false, nil
		}
		if err != nil {
			return 0, 0, false, err
		}
	}
	var sourceID int64
	var sourceType, kind, metadata string
	var observed int
	err := tx.QueryRowContext(ctx, `SELECT c.source_id, s.source_type, COALESCE(c.conversation_type,''),
 COALESCE(c.participant_count,0), COALESCE(CAST(c.metadata AS TEXT),'') FROM conversations c
 JOIN sources s ON s.id = c.source_id WHERE c.id = ?`, conversationID).
		Scan(&sourceID, &sourceType, &kind, &observed, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("read appreciation conversation: %w", err)
	}
	record := decodeMembershipRecord(metadata)
	if kind != string(MessageExportConversationDirectChat) || !personSweepSourceAuthenticatesSender(sourceType) ||
		!record.counted || record.unknown || record.repairPending {
		return 0, 0, false, nil
	}
	rosterQuery := `SELECT cp.participant_id,
 EXISTS(SELECT 1 FROM person_participants pp WHERE pp.person_id = ? AND pp.participant_id = cp.participant_id)
 FROM conversation_participants cp WHERE cp.conversation_id = ? ORDER BY cp.participant_id LIMIT 3`
	if lock && s.dialect.DriverName() == postgresDriverName {
		rosterQuery += ` FOR UPDATE OF cp`
	}
	rows, err := tx.QueryContext(ctx, rosterQuery, personID, conversationID)
	if err != nil {
		return 0, 0, false, err
	}
	var total, peers int
	var ownerID int64
	for rows.Next() {
		var participantID int64
		var belongs bool
		if err := rows.Scan(&participantID, &belongs); err != nil {
			_ = rows.Close()
			return 0, 0, false, err
		}
		total++
		if belongs {
			peers++
		} else {
			ownerID = participantID
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, false, err
	}
	if total != 2 || peers != 1 || ownerID == 0 || attachmentPolicyParticipantCount(sourceType, max(observed, total), metadata) != 2 {
		return 0, 0, false, nil
	}
	return sourceID, ownerID, true, nil
}

const appreciationOwnerMessagePredicate = `m.source_is_from_me = TRUE AND COALESCE(m.is_sent,FALSE) = TRUE AND m.draft_authored = FALSE AND m.sent_at IS NOT NULL`

// appreciationOwnerCurrentTx admits owner text only if the source's complete
// roster still proves the same two-person conversation and the exact native
// owner message remains later than the peer contribution.
func (s *Store) appreciationOwnerCurrentTx(ctx context.Context, tx *loggedTx, personID int64, memory peoplesweep.BriefAppreciationMemory, profile peoplesweep.ProviderProfile, peerTime time.Time, lock bool) (bool, error) {
	if memory.OwnerRef == nil || memory.Suggestion.Thanked == nil || memory.Suggestion.Thanked.Quote == "" ||
		memory.OwnerRef.SourceLane != peoplesweep.SourceConversationText || memory.OwnerRef.SourceID != memory.PeerRef.SourceID {
		return false, nil
	}
	after, before, err := sweepDateBounds(profile.SourceSince, profile.SourceUntil)
	if err != nil {
		return false, err
	}
	sourceID, ownerID, ok, err := s.appreciationDirectOwnerTx(ctx, tx, personID, memory.ConversationID, lock)
	if err != nil || !ok || sourceID != memory.OwnerRef.SourceID {
		return false, err
	}
	var nativeID string
	var sent requiredTimestamp
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(m.source_message_id,''), m.sent_at FROM messages m
 WHERE m.id = ? AND m.source_id = ? AND m.conversation_id = ? AND m.sender_id = ? AND `+LiveMessagesWhere("m", true)+`
 AND `+appreciationOwnerMessagePredicate,
		memory.OwnerRef.MessageID, sourceID, memory.ConversationID, ownerID).Scan(&nativeID, &sent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read appreciation owner message: %w", err)
	}
	if nativeID == "" || nativeID != memory.OwnerRef.SourceMessageID || !sent.Time.After(peerTime) ||
		after != nil && sent.Time.Before(*after) || before != nil && !sent.Time.Before(*before) {
		return false, nil
	}
	var body sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`, memory.OwnerRef.MessageID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read appreciation owner body: %w", err)
	}
	return body.Valid && sweepSHA256(body.String) == memory.OwnerRawSHA256 && strings.Contains(body.String, memory.Suggestion.Thanked.Quote), nil
}
