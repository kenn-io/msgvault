package store

import (
	"context"
	"fmt"
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
		key := NormalizeIdentifierForCompare(address)
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
		query += `) ORDER BY pp.person_id`
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
		for _, id := range people {
			names, err := s.ListPersonNamesContext(ctx, id, true)
			if err != nil {
				return nil, err
			}
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
					if value != "" && !slices.Contains(result[key], value) {
						result[key] = append(result[key], value)
					}
				}
			}
		}
	}
	return result, nil
}
