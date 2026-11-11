package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

func (s *Store) PersonSweepAppreciationDismissed(ctx context.Context, keys []string) (map[string]bool, error) {
	states, err := s.GetPersonAppreciationStatesContext(ctx, keys)
	if err != nil {
		return nil, err
	}
	dismissed := make(map[string]bool, len(states))
	for key, state := range states {
		dismissed[key] = state.DismissedAt != nil
	}
	return dismissed, nil
}

// PersonSweepAppreciationPeers resolves exact body text only for peer messages
// already admitted to a brief window. The normalized fact excerpt is never
// used as an exact quotation, and bodies are fetched by primary key.
func (s *Store) PersonSweepAppreciationPeers(
	ctx context.Context, personID int64, items []peoplesweep.EvidenceItem, throughSequence int64,
) ([]peoplesweep.BriefAppreciationPeer, error) {
	peers := make([]peoplesweep.BriefAppreciationPeer, 0, len(items))
	for _, candidate := range items {
		if candidate.SourceClass != peoplesweep.SourceConversationText || candidate.PersonID != personID ||
			candidate.SubjectPersonID == nil || *candidate.SubjectPersonID != personID || candidate.Directness != personfacts.DirectSelf {
			continue
		}
		current, err := s.HydratePersonSweepMessages(ctx, personID, []int64{candidate.Ref.MessageID}, throughSequence)
		if errors.Is(err, peoplesweep.ErrPersonSweepMessageUnavailable) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("rehydrate appreciation peer: %w", err)
		}
		if len(current) != 1 || current[0].Ref != candidate.Ref || current[0].ContentSHA256 != candidate.ContentSHA256 ||
			current[0].SubjectPersonID == nil || *current[0].SubjectPersonID != personID || current[0].Directness != personfacts.DirectSelf {
			continue
		}
		var peer peoplesweep.BriefAppreciationPeer
		err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
			key, err := appreciationMessageKeyTx(ctx, tx, candidate.Ref.MessageID)
			if err != nil || key == "" {
				return err
			}
			var body sql.NullString
			var sourceID int64
			err = tx.QueryRowContext(ctx, `SELECT m.source_id, m.conversation_id FROM messages m WHERE m.id = ? AND `+
				LiveMessagesWhere("m", true), candidate.Ref.MessageID).Scan(&sourceID, &peer.ConversationID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if sourceID != candidate.Ref.SourceID {
				return nil
			}
			err = tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`, candidate.Ref.MessageID).Scan(&body)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if !body.Valid || body.String == "" || !utf8.ValidString(body.String) {
				return nil
			}
			peer.ContributionKey = key
			peer.Ref = candidate.Ref
			peer.EventTime = candidate.EventTime.UTC()
			peer.RawSHA256 = sweepSHA256(body.String)
			peer.ExactExcerpt = truncateSweepRunes(body.String, 1000)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("load appreciation peer: %w", err)
		}
		if peer.ContributionKey != "" {
			peers = append(peers, peer)
		}
	}
	return peers, nil
}

// PersonSweepAppreciationOwners loads only positively native-owner messages
// from a complete direct roster shared with an admitted peer. The provider's
// membership count establishes completeness; accumulated rows alone do not.
func (s *Store) PersonSweepAppreciationOwners(
	ctx context.Context, personID int64, peers []peoplesweep.BriefAppreciationPeer,
	profile peoplesweep.ProviderProfile, throughSequence int64, limit int,
) ([]peoplesweep.BriefAppreciationOwner, error) {
	owners := []peoplesweep.BriefAppreciationOwner{}
	if !profile.AllowSensitive || !sweepSourceAllowed(profile.AllowedSources, peoplesweep.SourceConversationText) || limit <= 0 {
		return owners, nil
	}
	limit = min(limit, maxPersonSweepEvidenceRows)
	after, before, err := sweepDateBounds(profile.SourceSince, profile.SourceUntil)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool)
	for _, peer := range peers {
		if seen[peer.ConversationID] || len(owners) >= limit {
			continue
		}
		seen[peer.ConversationID] = true
		err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
			var sourceID int64
			var sourceType, conversationType, metadata string
			var participantCount int
			err := tx.QueryRowContext(ctx, `SELECT c.source_id, s.source_type,
 COALESCE(c.conversation_type,''), COALESCE(c.participant_count,0), COALESCE(CAST(c.metadata AS TEXT),'')
 FROM conversations c JOIN sources s ON s.id = c.source_id WHERE c.id = ?`, peer.ConversationID).
				Scan(&sourceID, &sourceType, &conversationType, &participantCount, &metadata)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if sourceID != peer.Ref.SourceID || conversationType != "direct_chat" || !personSweepSourceAuthenticatesSender(sourceType) {
				return nil
			}
			record := decodeMembershipRecord(metadata)
			if !record.counted || record.unknown || record.repairPending {
				return nil
			}
			rows, err := tx.QueryContext(ctx, `SELECT cp.participant_id,
 EXISTS(SELECT 1 FROM person_participants pp WHERE pp.person_id = ? AND pp.participant_id = cp.participant_id)
 FROM conversation_participants cp WHERE cp.conversation_id = ? ORDER BY cp.participant_id LIMIT 3`, personID, peer.ConversationID)
			if err != nil {
				return err
			}
			var rosterCount, boundCount int
			var ownerParticipantID int64
			for rows.Next() {
				var participantID int64
				var belongs bool
				if err := rows.Scan(&participantID, &belongs); err != nil {
					_ = rows.Close()
					return err
				}
				rosterCount++
				if belongs {
					boundCount++
				} else {
					ownerParticipantID = participantID
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if rosterCount != 2 || boundCount != 1 || ownerParticipantID == 0 ||
				attachmentPolicyParticipantCount(sourceType, max(participantCount, rosterCount), metadata) != 2 {
				return nil
			}
			boundSQL, boundArgs := personSweepJournalBoundPredicate(personID, throughSequence)
			args := []any{peer.ConversationID, sourceID, ownerParticipantID}
			where := ""
			if after != nil {
				where += " AND m.sent_at >= ?"
				args = append(args, *after)
			}
			if before != nil {
				where += " AND m.sent_at < ?"
				args = append(args, *before)
			}
			args = append(args, boundArgs...)
			args = append(args, limit-len(owners))
			rows, err = tx.QueryContext(ctx, `SELECT m.id, COALESCE(m.source_message_id,''), m.sent_at
 FROM messages m WHERE m.conversation_id = ? AND m.source_id = ? AND m.sender_id = ? AND `+LiveMessagesWhere("m", true)+`
 AND m.source_is_from_me = TRUE AND COALESCE(m.is_sent, FALSE) = TRUE AND m.draft_authored = FALSE
 AND m.sent_at IS NOT NULL`+where+boundSQL+` ORDER BY m.sent_at DESC, m.id DESC LIMIT ?`, args...)
			if err != nil {
				return err
			}
			type ownerRow struct {
				id              int64
				sourceMessageID string
				sent            time.Time
			}
			var candidates []ownerRow
			for rows.Next() {
				var row ownerRow
				var sent requiredTimestamp
				if err := rows.Scan(&row.id, &row.sourceMessageID, &sent); err != nil {
					_ = rows.Close()
					return err
				}
				row.sent = sent.Time.UTC()
				candidates = append(candidates, row)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			for _, row := range candidates {
				if row.sourceMessageID == "" {
					continue
				}
				var body sql.NullString
				err := tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`, row.id).Scan(&body)
				if errors.Is(err, sql.ErrNoRows) || err == nil && (!body.Valid || body.String == "" || !utf8.ValidString(body.String)) {
					continue
				}
				if err != nil {
					return err
				}
				hash := sweepSHA256(body.String)
				owners = append(owners, peoplesweep.BriefAppreciationOwner{
					EvidenceID: "owner:" + sweepSHA256(fmt.Sprintf("%d/%d/%s", sourceID, row.id, hash)),
					Ref: peoplesweep.EvidenceRef{SourceLane: peoplesweep.SourceConversationText, SourceID: sourceID,
						MessageID: row.id, SourceMessageID: row.sourceMessageID},
					ConversationID: peer.ConversationID, RawSHA256: hash,
					ExactExcerpt: truncateSweepRunes(body.String, 1000), EventTime: row.sent,
				})
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("load appreciation owner context: %w", err)
		}
	}
	return owners, nil
}
