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
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
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
	r := s.meetingReader(ctx)
	for start := 0; start < len(addresses); start += 500 {
		batch := addresses[start:min(start+500, len(addresses))]
		args := make([]any, 0, len(batch)*2)
		for _, email := range batch {
			args = append(args, email)
		}
		args = append(args, args...)
		query := `WITH matches AS (
 SELECT LOWER(email_address) AS address,id AS participant_id FROM participants WHERE LOWER(email_address) IN (` + placeholders(len(batch)) + `)
 UNION
 SELECT LOWER(identifier_value) AS address,participant_id FROM participant_identifiers WHERE LOWER(identifier_value) IN (` + placeholders(len(batch)) + `)
 ) SELECT address,participant_id,COALESCE((SELECT person_id FROM person_participants pp WHERE pp.participant_id=matches.participant_id),0) FROM matches WHERE 1=1`
		if sourcescope.Restricted(ctx) {
			scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
			query += ` AND EXISTS (SELECT 1 FROM messages m WHERE m.deleted_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id=m.id)
 AND (m.sender_id=matches.participant_id OR EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=matches.participant_id))` + scope + `)`
			args = append(args, scopeArgs...)
		}
		rows, err := r.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("look up meeting attendees: %w", err)
		}
		defer func() { _ = rows.Close() }()
		ambiguous := map[string]bool{}
		for rows.Next() {
			var address string
			var participant AddressParticipant
			if err := rows.Scan(&address, &participant.ParticipantID, &participant.PersonID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if old, ok := result[address]; ok && old.ParticipantID != participant.ParticipantID {
				ambiguous[address] = true
			}
			result[address] = participant
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		for address := range ambiguous {
			delete(result, address)
		}
	}
	return result, nil
}

// MeetingActionItemCountsContext counts all archived action items for each
// requested active transcript. Unauthorized and unknown IDs are absent.
func (s *Store) MeetingActionItemCountsContext(ctx context.Context, transcriptIDs []int64) (map[int64]int, error) {
	result := map[int64]int{}
	ids := slices.Clone(transcriptIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return result, ctx.Err()
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader(ctx)
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
		args = append(args, scopeArgs...)
		rows, err := r.QueryContext(ctx, `SELECT m.id,COUNT(a.message_id) FROM messages m LEFT JOIN meeting_action_items a ON a.message_id=m.id
 WHERE m.id IN (`+placeholders(len(batch))+`) AND m.message_type='meeting_transcript' AND m.deleted_at IS NULL`+scope+` GROUP BY m.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("count meeting action items: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var count int
			if err := rows.Scan(&id, &count); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result[id] = count
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return result, nil
}
