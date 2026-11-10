package store

import (
	"context"
	"errors"
	"net/mail"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/draftlint"
)

// CuratedNamesForAddressesContext resolves only curated person names. A nil
// source scope is unrestricted; an empty non-nil scope admits no identities.
func (s *Store) CuratedNamesForAddressesContext(ctx context.Context, addresses []string, sourceIDs []int64) (map[string][]draftlint.Name, error) {
	result := make(map[string][]draftlint.Name)
	if sourceIDs != nil && len(sourceIDs) == 0 {
		return result, nil
	}
	for _, address := range addresses {
		parsed, err := mail.ParseAddress(address)
		if err != nil {
			continue
		}
		key := NormalizeIdentifierForCompare(parsed.Address)
		participantID, err := s.EmailParticipantContext(ctx, parsed.Address)
		if err != nil {
			return nil, err
		}
		if participantID == 0 {
			continue
		}
		if sourceIDs != nil {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sourceIDs)), ",")
			query := `SELECT EXISTS (SELECT 1 FROM participants p WHERE p.id = ? AND (
 EXISTS (SELECT 1 FROM messages m WHERE m.sender_id = p.id AND m.source_id IN (` + placeholders + `)) OR EXISTS (
 SELECT 1 FROM message_recipients mr WHERE mr.participant_id = p.id AND EXISTS (
 SELECT 1 FROM messages m WHERE m.id = mr.message_id AND m.source_id IN (` + placeholders + `))) OR EXISTS (
 SELECT 1 FROM conversation_participants cp WHERE cp.participant_id = p.id AND EXISTS (
 SELECT 1 FROM conversations c WHERE c.id = cp.conversation_id AND c.source_id IN (` + placeholders + `)))))`
			args := []any{participantID}
			for range 3 {
				for _, id := range sourceIDs {
					args = append(args, id)
				}
			}
			var visible bool
			if err := s.db.QueryRowContext(ctx, query, args...).Scan(&visible); err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
		}
		person, err := s.PersonForParticipantsContext(ctx, []int64{participantID})
		if errors.Is(err, ErrPersonBindingConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if person == nil {
			continue
		}
		result[key], err = s.curatedDraftNamesContext(ctx, person.ID)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) curatedDraftNamesContext(ctx context.Context, personID int64) ([]draftlint.Name, error) {
	names, err := s.ListPersonNamesContext(ctx, personID, true)
	if err != nil {
		return nil, err
	}
	var result []draftlint.Name
	for _, name := range names {
		var values []draftlint.Name
		if name.GivenName != nil {
			values = append(values, draftlint.Name{Text: *name.GivenName, Given: true})
		}
		if name.Formatted != nil {
			switch name.NameKind {
			case PersonNameNickname:
				values = append(values, draftlint.Name{Text: *name.Formatted})
			case PersonNameFormatted:
				if tokens := strings.Fields(*name.Formatted); len(tokens) > 0 {
					values = append(values, draftlint.Name{Text: tokens[0]})
				}
			}
		}
		for _, value := range values {
			value.Text = strings.TrimSpace(value.Text)
			if value.Text != "" && !slices.Contains(result, value) {
				result = append(result, value)
			}
		}
	}
	return result, nil
}
