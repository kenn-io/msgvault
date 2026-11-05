package store

import (
	"context"
	"database/sql"
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
	query := `SELECT m.id,md.projection_version,COUNT(a.message_id) FROM messages m LEFT JOIN meeting_details md ON md.message_id=m.id LEFT JOIN meeting_action_items a ON a.message_id=m.id
 WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL` + scope + ` AND m.id IN (%s) GROUP BY m.id,md.projection_version`
	err = queryInChunksContext(ctx, snapshot.tx, ids, args, query, func(rows *loggedRows) error {
		var id int64
		var count int
		var version sql.NullInt64
		if err := rows.Scan(&id, &version, &count); err != nil {
			return err
		}
		if err := validateMeetingProjectionVersion(version); err != nil {
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
