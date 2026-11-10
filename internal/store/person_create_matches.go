package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
)

type PersonIfExists string

const (
	PersonIfExistsError  PersonIfExists = "error"
	PersonIfExistsReturn PersonIfExists = "return"
)

type PersonEnsureStatus string

const (
	PersonEnsureCreated         PersonEnsureStatus = "created"
	PersonEnsureMatchedExisting PersonEnsureStatus = "matched_existing"
)

type PersonMatchBasis struct {
	Kind          ContactAddressKind `json:"kind"`
	Value         string             `json:"value"`
	Matched       string             `json:"matched" enum:"contact_point,participant,identifier,observation"`
	PersonID      *int64             `json:"person_id" nullable:"true" required:"false"`
	ParticipantID *int64             `json:"participant_id" nullable:"true" required:"false"`
}

type PersonUnappliedContact struct {
	Kind  ContactAddressKind `json:"kind"`
	Value string             `json:"value"`
	Type  string             `json:"type,omitzero"`
}

type PersonEnsureInfo struct {
	Status            PersonEnsureStatus       `json:"status" enum:"created,matched_existing"`
	MatchBasis        []PersonMatchBasis       `json:"match_basis"`
	UnappliedContacts []PersonUnappliedContact `json:"unapplied_contacts"`
}

type PersonCreateCandidate struct {
	PersonID      *int64 `json:"person_id" nullable:"true" required:"false"`
	ParticipantID *int64 `json:"participant_id" nullable:"true" required:"false"`
	Name          string `json:"name"`
}

type PersonAmbiguousError struct {
	Candidates []PersonCreateCandidate `json:"candidates"`
	MatchBasis []PersonMatchBasis      `json:"match_basis"`
}

func (e *PersonAmbiguousError) Error() string {
	return "contacts match multiple people or participant clusters"
}

// PersonCreateNext is promotion guidance for an unbound identity cluster.
// It is local to creation; the candidates list is a separate capability.
type PersonCreateNext struct {
	CLI []string            `json:"cli"`
	MCP PersonCreateMCPNext `json:"mcp"`
}

type PersonCreateMCPNext struct {
	Tool      string           `json:"tool"`
	Arguments map[string]int64 `json:"arguments"`
}

type PersonMatchesParticipantError struct {
	ParticipantID int64              `json:"participant_id"`
	MatchBasis    []PersonMatchBasis `json:"match_basis"`
	Next          *PersonCreateNext  `json:"next"`
}

func (e *PersonMatchesParticipantError) Error() string {
	return fmt.Sprintf("contacts match participant %d; use msgvault person promote %d", e.ParticipantID, e.ParticipantID)
}

// collectPersonContactMatchesTx reads every stored representation, including
// service identifiers and current observations, without stopping at one person.
func (s *Store) collectPersonContactMatchesTx(ctx context.Context, tx *loggedTx, points []personCreateContactPoint) ([]PersonMatchBasis, error) {
	matches := []PersonMatchBasis{}
	edges, err := s.loadLinkEdgesTxContext(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, point := range points {
		primary := `LOWER(email_address) = ?`
		if point.kind == ContactAddressPhone {
			primary = `phone_number = ?`
		}
		rows, err := tx.QueryContext(ctx, `
   SELECT person_id, 0, 'contact_point' FROM person_contact_points
   WHERE address_kind = ? AND normalized_value = ? AND active_until IS NULL AND superseded_at IS NULL
   UNION
   SELECT 0, id, 'participant' FROM participants WHERE `+primary+`
   UNION
   SELECT 0, participant_id, 'identifier' FROM participant_identifiers WHERE LOWER(identifier_value) = ?
   UNION
   SELECT 0, participant_id, 'observation' FROM participant_contact_observations
   WHERE address_kind = ? AND normalized_value = ? AND active_until IS NULL AND superseded_at IS NULL
   ORDER BY 1, 2, 3`, point.kind, point.normalized, point.normalized, point.normalized, point.kind, point.normalized)
		if err != nil {
			return nil, fmt.Errorf("collect person contact matches: %w", err)
		}
		var found []PersonMatchBasis
		for rows.Next() {
			var personID, participantID int64
			match := PersonMatchBasis{Kind: point.kind, Value: point.normalized}
			if err := rows.Scan(&personID, &participantID, &match.Matched); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if personID > 0 {
				match.PersonID = new(personID)
			}
			if participantID > 0 {
				match.ParticipantID = new(participantID)
			}
			found = append(found, match)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read person contact matches: %w", err)
		}
		for _, match := range found {
			if match.ParticipantID == nil {
				matches = append(matches, match)
				continue
			}
			members := sortedComponentMembers(*match.ParticipantID, edges)
			ids, err := personIDsForParticipantsTx(ctx, tx, members)
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				matches = append(matches, match)
			} else {
				for _, id := range ids {
					bound := match
					bound.PersonID = new(id)
					matches = append(matches, bound)
				}
			}
		}
	}
	return matches, nil
}

func (s *Store) resolvePersonCreateMatchesTx(ctx context.Context, tx *loggedTx, points []personCreateContactPoint, matches []PersonMatchBasis) (*Person, error) {
	people, participants := map[int64]bool{}, map[int64]bool{}
	edges, err := s.loadLinkEdgesTxContext(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, match := range matches {
		if match.PersonID != nil {
			people[*match.PersonID] = true
		} else {
			participants[sortedComponentMembers(*match.ParticipantID, edges)[0]] = true
		}
	}
	if len(people) == 1 && len(participants) == 0 {
		var id int64
		for id = range people {
		}
		person, err := s.getPersonTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		info := &PersonEnsureInfo{Status: PersonEnsureMatchedExisting, MatchBasis: matches, UnappliedContacts: []PersonUnappliedContact{}}
		for _, point := range points {
			matched := false
			for _, match := range matches {
				if match.Kind == point.kind && match.Value == point.normalized {
					matched = true
					break
				}
			}
			if !matched {
				info.UnappliedContacts = append(info.UnappliedContacts, PersonUnappliedContact{Kind: point.kind, Value: point.value, Type: point.typeToken})
			}
		}
		person.Ensure = info
		return person, nil
	}
	if len(people) == 0 && len(participants) == 1 {
		for id := range participants {
			return nil, &PersonMatchesParticipantError{ParticipantID: id, MatchBasis: matches, Next: &PersonCreateNext{CLI: []string{"msgvault", "person", "promote", strconv.FormatInt(id, 10), "--json"}, MCP: PersonCreateMCPNext{Tool: "promote_person", Arguments: map[string]int64{"participant_id": id}}}}
		}
	}
	candidates := []PersonCreateCandidate{}
	for _, group := range []struct {
		ids   map[int64]bool
		table string
	}{{people, "persons"}, {participants, "participants"}} {
		ids := make([]int64, 0, len(group.ids))
		for id := range group.ids {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			candidate := PersonCreateCandidate{}
			if group.table == "persons" {
				candidate.PersonID = new(id)
			} else {
				candidate.ParticipantID = new(id)
			}
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(display_name, '') FROM `+group.table+` WHERE id = ?`, id).Scan(&candidate.Name); err != nil {
				return nil, fmt.Errorf("load creation match name: %w", err)
			}
			candidates = append(candidates, candidate)
		}
	}
	return nil, &PersonAmbiguousError{Candidates: candidates, MatchBasis: matches}
}
