package store

import (
	"context"
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
