package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/personscope"
)

func normalizeDocumentPersonScope(scope *personscope.Scope) (*personscope.Scope, error) {
	if scope == nil {
		return nil, nil //nolint:nilnil // An omitted optional scope has no value and no error.
	}
	normalized := *scope
	normalized.ParticipantIDs = slices.Clone(scope.ParticipantIDs)
	slices.Sort(normalized.ParticipantIDs)
	normalized.ParticipantIDs = slices.Compact(normalized.ParticipantIDs)
	if len(normalized.ParticipantIDs) > 0 && normalized.ParticipantIDs[0] <= 0 {
		return nil, fmt.Errorf("%w: resolved person scope has invalid participants", ErrDocumentSearchInvalidRequest)
	}
	selected := make(map[personscope.Direction]bool, len(scope.Directions))
	for _, direction := range scope.Directions {
		switch direction {
		case personscope.FromPerson, personscope.ToPerson, personscope.Group:
			selected[direction] = true
		default:
			return nil, fmt.Errorf("%w: unknown person direction %q", ErrDocumentSearchInvalidRequest, direction)
		}
	}
	normalized.Directions = normalized.Directions[:0]
	for _, direction := range []personscope.Direction{personscope.FromPerson, personscope.ToPerson, personscope.Group} {
		if selected[direction] {
			normalized.Directions = append(normalized.Directions, direction)
		}
	}
	if len(normalized.Directions) == 0 {
		return nil, fmt.Errorf("%w: resolved person scope has no directions", ErrDocumentSearchInvalidRequest)
	}
	return &normalized, nil
}

func (s *Store) populateDocumentPersonProvenance(
	ctx context.Context,
	results []DocumentSearchResult,
	scope personscope.Scope,
) error {
	if len(results) == 0 {
		return nil
	}
	messageIDs := make([]int64, 0, len(results))
	for _, result := range results {
		messageIDs = append(messageIDs, result.MessageID)
	}
	provenance, err := s.PersonProvenanceForMessages(ctx, messageIDs, scope)
	if err != nil {
		return err
	}
	for i := range results {
		results[i].PersonProvenance = provenance[results[i].MessageID]
		if results[i].PersonProvenance == nil {
			return fmt.Errorf("document person provenance missing for message %d", results[i].MessageID)
		}
	}
	return nil
}

// PersonProvenanceForMessages hydrates the exact participant, role, and
// direction edges for a resolved scope over a bounded set of messages.
func (s *Store) PersonProvenanceForMessages(
	ctx context.Context,
	messageIDs []int64,
	scope personscope.Scope,
) (map[int64]*personscope.Provenance, error) {
	if len(messageIDs) == 0 {
		return map[int64]*personscope.Provenance{}, nil
	}
	if len(scope.ParticipantIDs) == 0 {
		return map[int64]*personscope.Provenance{}, nil
	}
	messageIDs = slices.Clone(messageIDs)
	slices.Sort(messageIDs)
	messageIDs = slices.Compact(messageIDs)
	messageRows := make([]string, len(messageIDs))
	participantRows := make([]string, len(scope.ParticipantIDs))
	args := make([]any, 0, len(messageIDs)+len(scope.ParticipantIDs))
	for i, id := range messageIDs {
		messageRows[i] = "(CAST(? AS BIGINT))"
		args = append(args, id)
	}
	for i, id := range scope.ParticipantIDs {
		participantRows[i] = "(CAST(? AS BIGINT))"
		args = append(args, id)
	}
	directionEvidence := `(m.is_from_me = TRUE OR m.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = m.id AND LOWER(known_from.recipient_type) = 'from'))`
	rosterCondition := `LOWER(COALESCE(c.conversation_type, '')) IN ('group_chat', 'channel')`
	if scope.IncludeUnclassifiedRosterRows {
		rosterCondition = `(LOWER(COALESCE(c.conversation_type, '')) <> 'direct_chat' OR NOT ` + directionEvidence + `)`
	}
	query := `WITH selected_messages(message_id) AS (VALUES ` + strings.Join(messageRows, ",") + `),
	person_ids(participant_id) AS (VALUES ` + strings.Join(participantRows, ",") + `),
	person_edges AS (
		SELECT mr.message_id, mr.participant_id, LOWER(mr.recipient_type) AS role
		FROM selected_messages sm
		JOIN message_recipients mr ON mr.message_id = sm.message_id
		JOIN person_ids p ON p.participant_id = mr.participant_id
		WHERE LOWER(mr.recipient_type) IN ('from', 'to', 'cc', 'bcc')
		UNION ALL
		SELECT m.id, m.sender_id, 'from'
		FROM selected_messages sm JOIN messages m ON m.id = sm.message_id
		JOIN person_ids p ON p.participant_id = m.sender_id
		UNION ALL
		SELECT m.id, cp.participant_id, 'to'
		FROM selected_messages sm JOIN messages m ON m.id = sm.message_id
		JOIN conversations c ON c.id = m.conversation_id
		JOIN conversation_participants cp ON cp.conversation_id = m.conversation_id
		JOIN person_ids p ON p.participant_id = cp.participant_id
		WHERE LOWER(COALESCE(c.conversation_type, '')) = 'direct_chat'
		  AND ` + directionEvidence + `
		  AND NOT EXISTS (
			SELECT 1 FROM person_ids sender_person
			WHERE sender_person.participant_id = m.sender_id OR (
				m.sender_id IS NULL AND EXISTS (
					SELECT 1 FROM message_recipients mr_from
					WHERE mr_from.message_id = m.id
					  AND LOWER(mr_from.recipient_type) = 'from'
					  AND mr_from.participant_id = sender_person.participant_id)))
		UNION ALL
		SELECT m.id, cp.participant_id, 'conversation_member'
		FROM selected_messages sm JOIN messages m ON m.id = sm.message_id
		JOIN conversations c ON c.id = m.conversation_id
		JOIN conversation_participants cp ON cp.conversation_id = m.conversation_id
		JOIN person_ids p ON p.participant_id = cp.participant_id
		WHERE ` + rosterCondition + `
	)
	SELECT message_id, participant_id, role FROM person_edges
	ORDER BY message_id, participant_id, role`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read person message provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type edgeSet map[personscope.Role]bool
	edges := make(map[int64]map[int64]edgeSet, len(messageIDs))
	for rows.Next() {
		var messageID, participantID int64
		var role personscope.Role
		if err := rows.Scan(&messageID, &participantID, &role); err != nil {
			return nil, fmt.Errorf("scan person message provenance: %w", err)
		}
		if edges[messageID] == nil {
			edges[messageID] = make(map[int64]edgeSet)
		}
		if edges[messageID][participantID] == nil {
			edges[messageID][participantID] = make(edgeSet)
		}
		edges[messageID][participantID][role] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person message provenance: %w", err)
	}
	result := make(map[int64]*personscope.Provenance, len(edges))
	for messageID, messageEdges := range edges {
		provenance := &personscope.Provenance{}
		for participantID := range messageEdges {
			provenance.ParticipantIDs = append(provenance.ParticipantIDs, participantID)
		}
		slices.Sort(provenance.ParticipantIDs)
		for _, role := range []personscope.Role{
			personscope.RoleFrom, personscope.RoleTo, personscope.RoleCC,
			personscope.RoleBCC, personscope.RoleConversationMember,
		} {
			for _, participantID := range provenance.ParticipantIDs {
				if messageEdges[participantID][role] {
					provenance.Roles = append(provenance.Roles, role)
					break
				}
			}
		}
		if slices.Contains(provenance.Roles, personscope.RoleFrom) {
			provenance.Directions = append(provenance.Directions, personscope.FromPerson)
		}
		if slices.Contains(provenance.Roles, personscope.RoleTo) || slices.Contains(provenance.Roles, personscope.RoleCC) || slices.Contains(provenance.Roles, personscope.RoleBCC) {
			provenance.Directions = append(provenance.Directions, personscope.ToPerson)
		}
		if slices.Contains(provenance.Roles, personscope.RoleConversationMember) {
			provenance.Directions = append(provenance.Directions, personscope.Group)
		}
		result[messageID] = provenance
	}
	return result, nil
}
