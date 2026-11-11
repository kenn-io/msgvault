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

func (s *Store) PersonSweepAppreciationRetired(ctx context.Context, personID int64, keys []string, profile peoplesweep.ProviderProfile) (map[string]bool, error) {
	var retired map[string]bool
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		retired, err = s.appreciationRetiredTx(ctx, tx, personID, keys, profile, false)
		return err
	})
	return retired, err
}

// appreciationRetiredTx checks explicit keys in one snapshot, then validates
// any exact proof in that same snapshot. No nested store transaction is opened.
func (s *Store) appreciationRetiredTx(ctx context.Context, tx *loggedTx, personID int64, keys []string, profile peoplesweep.ProviderProfile, lock bool) (map[string]bool, error) {
	retired := make(map[string]bool)
	states, err := getPersonAppreciationStates(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	for key, state := range states {
		if state.DismissedAt != nil {
			retired[key] = true
			continue
		}
		if state.ThanksBriefID == nil {
			continue
		}
		_, valid, err := s.validAppreciationProofTx(ctx, tx, personID, key, *state.ThanksBriefID, profile, lock)
		if err != nil {
			return nil, err
		}
		if valid {
			retired[key] = true
		}
	}
	return retired, nil
}

// PersonSweepAppreciationPeers resolves exact body text only for peer messages
// already admitted to a brief window. The normalized fact excerpt is never
// used as an exact quotation, and bodies are fetched by primary key.
func (s *Store) PersonSweepAppreciationPeers(
	ctx context.Context, personID int64, items []peoplesweep.EvidenceItem, throughSequence int64,
) ([]peoplesweep.BriefAppreciationPeer, error) {
	peers := make([]peoplesweep.BriefAppreciationPeer, 0, len(items))
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.Ref.MessageID)
	}
	current, _, err := s.hydratePersonSweepMessageSet(ctx, personID, peoplesweep.SourceConversationText, ids, throughSequence)
	if err != nil {
		return nil, fmt.Errorf("rehydrate appreciation peers: %w", err)
	}
	byID := make(map[int64]peoplesweep.EvidenceItem, len(current))
	for _, item := range current {
		byID[item.Ref.MessageID] = item
	}
	for _, candidate := range items {
		if candidate.SourceClass != peoplesweep.SourceConversationText || candidate.PersonID != personID ||
			candidate.SubjectPersonID == nil || *candidate.SubjectPersonID != personID || candidate.Directness != personfacts.DirectSelf {
			continue
		}
		current, ok := byID[candidate.Ref.MessageID]
		if !ok || current.Ref != candidate.Ref || current.ContentSHA256 != candidate.ContentSHA256 ||
			current.SubjectPersonID == nil || *current.SubjectPersonID != personID || current.Directness != personfacts.DirectSelf {
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
// Sampling is bounded and newest-first, so absence does not prove no thanks.
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
			sourceID, ownerParticipantID, ok, err := s.appreciationDirectOwnerTx(ctx, tx, personID, peer.ConversationID, false)
			if err != nil || !ok || sourceID != peer.Ref.SourceID {
				return err
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
			rows, err := tx.QueryContext(ctx, `SELECT m.id, COALESCE(m.source_message_id,''), m.sent_at
 FROM messages m WHERE m.conversation_id = ? AND m.source_id = ? AND m.sender_id = ? AND `+LiveMessagesWhere("m", true)+`
 AND `+appreciationOwnerMessagePredicate+where+boundSQL+` ORDER BY m.sent_at DESC, m.id DESC LIMIT ?`, args...)
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
