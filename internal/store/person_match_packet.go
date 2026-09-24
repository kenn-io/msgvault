package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/personmatch"
)

type PersonMatchPairSummaries struct {
	Left      personmatch.PairEndpoint
	Right     personmatch.PairEndpoint
	Truncated bool
}

// PersonMatchPairSummariesContext reads only the two participant identity
// summaries for a consented scoring packet. It never reads messages or notes.
func (s *Store) PersonMatchPairSummariesContext(ctx context.Context, leftID, rightID int64) (PersonMatchPairSummaries, error) {
	if leftID <= 0 || rightID <= 0 || leftID == rightID {
		return PersonMatchPairSummaries{}, errors.New("invalid person match participant pair")
	}
	result := PersonMatchPairSummaries{
		Left:  personmatch.PairEndpoint{Kind: "participant", Identifiers: []personmatch.PairIdentifier{}},
		Right: personmatch.PairEndpoint{Kind: "participant", Identifiers: []personmatch.PairIdentifier{}},
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`SELECT id, display_name, email_address, phone_number
		FROM participants WHERE id IN (?, ?)`), leftID, rightID)
	if err != nil {
		return result, fmt.Errorf("read person match pair: %w", err)
	}
	seen := map[int64]bool{}
	for rows.Next() {
		var id int64
		var name, email, phone sql.NullString
		if err := rows.Scan(&id, &name, &email, &phone); err != nil {
			_ = rows.Close()
			return result, err
		}
		var endpoint *personmatch.PairEndpoint
		if id == leftID {
			endpoint = &result.Left
		} else {
			endpoint = &result.Right
		}
		endpoint.DisplayName = boundedIdentityField(name.String, 128)
		endpoint.Email = boundedIdentityField(email.String, 256)
		endpoint.Phone = boundedIdentityField(phone.String, 64)
		seen[id] = true
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return result, err
	}
	if !seen[leftID] || !seen[rightID] {
		return result, ErrIdentityMatchEndpointNotFound
	}
	for _, participantID := range []int64{leftID, rightID} {
		rows, err = s.db.QueryContext(ctx, s.dialect.Rebind(`SELECT pi.participant_id, pi.identifier_type, pi.identifier_value,
		COALESCE(cs.slug, ''), COALESCE(pi.scope_kind, ''), COALESCE(pi.scope_value, '')
		FROM participant_identifiers pi LEFT JOIN communication_services cs ON cs.id = pi.service_id
		WHERE pi.participant_id = ? ORDER BY pi.id LIMIT 65`), participantID)
		if err != nil {
			return result, fmt.Errorf("read person match pair identifiers: %w", err)
		}
		identifierRows := 0
		for rows.Next() {
			identifierRows++
			var id int64
			var kind, value, serviceSlug, scopeKind, scopeValue string
			if err := rows.Scan(&id, &kind, &value, &serviceSlug, &scopeKind, &scopeValue); err != nil {
				_ = rows.Close()
				return result, err
			}
			if !scorableIdentifierType(kind) {
				continue
			}
			var endpoint *personmatch.PairEndpoint
			if id == leftID {
				endpoint = &result.Left
			} else {
				endpoint = &result.Right
			}
			if len(endpoint.Identifiers) >= 8 {
				result.Truncated = true
				continue
			}
			endpoint.Identifiers = append(endpoint.Identifiers, personmatch.PairIdentifier{
				Type: kind, Value: boundedIdentityField(value, 256), ServiceSlug: serviceSlug,
				ScopeKind: scopeKind, ScopeValue: boundedIdentityField(scopeValue, 128),
			})
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return result, err
		}
		if identifierRows >= 65 {
			result.Truncated = true
		}
	}
	return result, err
}

func boundedIdentityField(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

func scorableIdentifierType(kind string) bool {
	switch kind {
	case "email", "phone", "beeper", "apple_id", "whatsapp", "imessage":
		return true
	default:
		return false
	}
}
