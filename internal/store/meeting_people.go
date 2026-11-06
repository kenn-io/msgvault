package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/sourcescope"
)

// AddressParticipant is an existing archive identity for an attendee address.
type AddressParticipant struct{ ParticipantID, PersonID int64 }

// ParticipantsByEmailContext maps normalized addresses to existing participants.
// Unknown or ambiguous addresses are absent; no identity is created. A scoped
// caller sees only participants observed in its authorized, active messages.
func (s *Store) ParticipantsByEmailContext(ctx context.Context, emails []string) (map[string]AddressParticipant, error) {
	result := map[string]AddressParticipant{}
	unique := map[string]bool{}
	for _, email := range emails {
		if email = strings.TrimSpace(email); email != "" {
			unique[email] = true
		}
	}
	addresses := make([]string, 0, len(unique))
	for email := range unique {
		addresses = append(addresses, email)
	}
	slices.Sort(addresses)
	if len(addresses) == 0 {
		return result, ctx.Err()
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	snapshot, _ := ctx.Value(readSnapshotKey{}).(*readSnapshot)
	scope := ""
	var scopeArgs []any
	if sourcescope.Restricted(ctx) {
		clause, args := sourceScopeClause(ctx, "m.source_id")
		scope = ` AND EXISTS (SELECT 1 FROM messages m WHERE m.deleted_at IS NULL
 AND (m.sender_id=p.id OR EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=p.id))` + clause + `)`
		scopeArgs = args
	}
	return emailParticipantsContext(ctx, snapshot.tx, addresses, scope, scopeArgs)
}

// MeetingActionItemCountsContext counts all archived action items for each
// requested active transcript. Unauthorized and unknown IDs are absent.
func (s *Store) MeetingActionItemCountsContext(ctx context.Context, transcriptIDs []int64) (map[int64]int, error) {
	result := map[int64]int{}
	ids := normalizedSignedIDs(transcriptIDs)
	if len(ids) == 0 {
		return result, ctx.Err()
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	snapshot, _ := ctx.Value(readSnapshotKey{}).(*readSnapshot)
	scope, args := sourceScopeClause(ctx, "m.source_id")
	query := `SELECT m.id,COUNT(a.message_id) FROM messages m LEFT JOIN meeting_action_items a ON a.message_id=m.id
 WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL` + scope + ` AND m.id IN (%s) GROUP BY m.id`
	err = queryInChunksContext(ctx, snapshot.tx, ids, args, query, func(rows *loggedRows) error {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return err
		}
		result[id] = count
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("count meeting action items: %w", err)
	}
	return result, nil
}

func emailParticipantsContext(ctx context.Context, q chunkQuerier, addresses []string, scope string, scopeArgs []any) (map[string]AddressParticipant, error) {
	result := map[string]AddressParticipant{}
	// Bind the scope before the requested addresses for the shared chunk reader.
	query := `WITH visible AS (SELECT p.id FROM participants p WHERE 1=1` + scope + `),
 requested(address) AS (VALUES %s), matches AS (SELECT r.address,p.id FROM requested r JOIN participants p ON LOWER(p.email_address)=LOWER(r.address)
 UNION SELECT r.address,p.participant_id FROM requested r JOIN participant_identifiers p ON LOWER(p.identifier_value)=LOWER(r.address))
 SELECT a.address,p.id,COALESCE(pp.person_id,0) FROM matches a
 JOIN visible p ON p.id=a.id LEFT JOIN person_participants pp ON pp.participant_id=p.id`
	ambiguous := map[string]bool{}
	err := queryInChunksWithValueExprContext(ctx, q, addresses, scopeArgs, query, "(?)", func(rows *loggedRows) error {
		var address string
		var participant AddressParticipant
		if err := rows.Scan(&address, &participant.ParticipantID, &participant.PersonID); err != nil {
			return err
		}
		if previous, ok := result[strings.ToLower(address)]; ok && previous.ParticipantID != participant.ParticipantID {
			ambiguous[strings.ToLower(address)] = true
		}
		result[strings.ToLower(address)] = participant
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("look up email participants: %w", err)
	}
	for address := range ambiguous {
		delete(result, address)
	}
	return result, nil
}
