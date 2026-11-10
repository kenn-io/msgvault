package store

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
)

// CuratedNamesForAddressesContext resolves only curated person names. A nil
// source scope is unrestricted; an empty non-nil scope admits no identities.
func (s *Store) CuratedNamesForAddressesContext(ctx context.Context, addresses []string, sourceIDs []int64) (map[string][]string, error) {
	result := make(map[string][]string)
	if sourceIDs != nil && len(sourceIDs) == 0 {
		return result, nil
	}
	for _, address := range addresses {
		parsed, err := mail.ParseAddress(address)
		if err != nil {
			continue
		}
		key := NormalizeIdentifierForCompare(parsed.Address)
		query := `SELECT pp.person_id FROM person_participants pp WHERE EXISTS (
 SELECT 1 FROM participants p WHERE p.id = pp.participant_id AND
 (LOWER(p.email_address) = ? OR EXISTS (SELECT 1 FROM participant_identifiers pi
 WHERE pi.participant_id = p.id AND LOWER(pi.identifier_value) = ?))`
		args := []any{key, key}
		if sourceIDs != nil {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sourceIDs)), ",")
			query += ` AND (EXISTS (SELECT 1 FROM messages m WHERE m.sender_id = p.id AND m.source_id IN (` + placeholders + `)) OR EXISTS (
 SELECT 1 FROM message_recipients mr WHERE mr.participant_id = p.id AND EXISTS (
 SELECT 1 FROM messages m WHERE m.id = mr.message_id AND m.source_id IN (` + placeholders + `))) OR EXISTS (
 SELECT 1 FROM conversation_participants cp WHERE cp.participant_id = p.id AND EXISTS (
 SELECT 1 FROM conversations c WHERE c.id = cp.conversation_id AND c.source_id IN (` + placeholders + `))))`
			for range 3 {
				for _, id := range sourceIDs {
					args = append(args, id)
				}
			}
		}
		query += `) GROUP BY pp.person_id ORDER BY pp.person_id LIMIT 2`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("resolve draft recipient people: %w", err)
		}
		var people []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			people = append(people, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if len(people) != 1 {
			continue // Ambiguous aliases cannot supply a greeting match.
		}
		result[key], err = s.curatedDraftNamesContext(ctx, people[0])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) curatedDraftNamesContext(ctx context.Context, personID int64) ([]string, error) {
	names, err := s.ListPersonNamesContext(ctx, personID, true)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, name := range names {
		var values []string
		if name.GivenName != nil {
			values = append(values, *name.GivenName)
		}
		if name.Formatted != nil {
			switch name.NameKind {
			case PersonNameNickname:
				values = append(values, *name.Formatted)
			case PersonNameFormatted:
				if tokens := strings.Fields(*name.Formatted); len(tokens) > 0 {
					values = append(values, tokens[0])
				}
			}
		}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" && !slices.Contains(result, value) {
				result = append(result, value)
			}
		}
	}
	return result, nil
}

// DraftConversationParticipantIDsContext reads active participants of an archived
// direct chat. Callers must first authorize read access to this source.
func (s *Store) DraftConversationParticipantIDsContext(ctx context.Context, sourceID int64, nativeID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cp.participant_id FROM conversation_participants cp
 WHERE cp.left_at IS NULL AND EXISTS (SELECT 1 FROM conversations c
 WHERE c.id = cp.conversation_id AND c.source_id = ? AND c.source_conversation_id = ?
 AND c.conversation_type = 'direct_chat') ORDER BY cp.participant_id`, sourceID, nativeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CuratedNamesForParticipantsContext uses curated person bindings, without
// inferring names from a provider's display labels or email aliases.
func (s *Store) CuratedNamesForParticipantsContext(ctx context.Context, participantIDs []int64) (map[string][]string, error) {
	result := make(map[string][]string)
	for _, id := range participantIDs {
		person, err := s.PersonForParticipantsContext(ctx, []int64{id})
		if errors.Is(err, ErrPersonBindingConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if person == nil {
			continue
		}
		names, err := s.curatedDraftNamesContext(ctx, person.ID)
		if err != nil {
			return nil, err
		}
		result[fmt.Sprint(id)] = names
	}
	return result, nil
}
