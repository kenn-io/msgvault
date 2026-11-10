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
func (s *Store) CuratedNamesForAddressesContext(ctx context.Context, addresses []string, sourceIDs []int64) ([]draftlint.Name, error) {
	var result []draftlint.Name
	if sourceIDs != nil && len(sourceIDs) == 0 {
		return result, nil
	}
	for _, address := range addresses {
		parsed, err := mail.ParseAddress(address)
		if err != nil {
			continue
		}
		participantID, err := s.EmailParticipantContext(ctx, parsed.Address)
		if err != nil {
			return nil, err
		}
		if participantID == 0 {
			continue
		}
		if sourceIDs != nil {
			evidence, err := s.SenderRelationships(ctx, []string{parsed.Address}, sourceIDs)
			if err != nil {
				return nil, err
			}
			if !evidence[strings.ToLower(strings.TrimSpace(parsed.Address))].SeenInArchive {
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
		names, err := s.curatedDraftNamesContext(ctx, person.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, names...)
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
		if name.NameKind == PersonNameStructured && name.GivenName != nil {
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
