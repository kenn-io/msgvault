package store

import (
	"context"
	"fmt"
	"maps"
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

const (
	PersonCreateCodeAmbiguous          = "person_ambiguous"
	PersonCreateCodeMatchesParticipant = "person_matches_participant"
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

// These predicates are shared by refusal and return mode. Refusal still
// selects curated contacts first and the lowest observed participant second.
const personCreateCuratedPredicate = `c.address_kind = ? AND c.normalized_value = ?
    AND c.active_until IS NULL AND c.superseded_at IS NULL`

func personCreateObservedMatchesSQL(kind ContactAddressKind) string {
	primary := `LOWER(email_address) = ?`
	if kind == ContactAddressPhone {
		primary = `phone_number = ?`
	}
	return `SELECT id AS participant_id, 'participant' AS matched FROM participants WHERE ` + primary + `
    UNION
    SELECT participant_id, 'identifier' FROM participant_identifiers WHERE LOWER(identifier_value) = ?
    UNION
    SELECT participant_id, 'observation' FROM participant_contact_observations
    WHERE address_kind = ? AND normalized_value = ? AND active_until IS NULL AND superseded_at IS NULL`
}

type personCreateMatches struct {
	basis                []PersonMatchBasis
	people, participants map[int64]bool
}

// collectPersonContactMatchesTx reads every stored representation. Link state
// is loaded only on an observed hit, and each component is resolved once.
func (s *Store) collectPersonContactMatchesTx(ctx context.Context, tx *loggedTx, points []personCreateContactPoint) (*personCreateMatches, error) {
	matches := &personCreateMatches{basis: []PersonMatchBasis{}, people: map[int64]bool{}, participants: map[int64]bool{}}
	type cluster struct {
		canonical int64
		people    []int64
	}
	var adj map[int64][]int64
	resolved := map[int64]*cluster{}
	resolve := func(id int64) (*cluster, error) {
		if result := resolved[id]; result != nil {
			return result, nil
		}
		if adj == nil {
			edges, err := s.loadLinkEdgesTxContext(ctx, tx)
			if err != nil {
				return nil, err
			}
			adj = buildAdjacency(edges)
		}
		members := slices.Sorted(maps.Keys(componentOfAdj(id, adj)))
		people, err := personIDsForParticipantsTx(ctx, tx, members)
		if err != nil {
			return nil, err
		}
		result := &cluster{canonical: members[0], people: people}
		for _, member := range members {
			resolved[member] = result
		}
		return result, nil
	}
	for _, point := range points {
		rows, err := tx.QueryContext(ctx, `
            SELECT c.person_id, 0, 'contact_point' FROM person_contact_points c WHERE `+personCreateCuratedPredicate+`
            UNION
            SELECT 0, participant_id, matched FROM (`+personCreateObservedMatchesSQL(point.kind)+`) observed
            ORDER BY 1, 2, 3`, point.kind, point.normalized, point.normalized, point.normalized, point.kind, point.normalized)
		if err != nil {
			return nil, fmt.Errorf("collect person contact matches: %w", err)
		}
		var found []PersonMatchBasis
		curated := false
		for rows.Next() {
			var personID, participantID int64
			match := PersonMatchBasis{Kind: point.kind, Value: point.normalized}
			if err := rows.Scan(&personID, &participantID, &match.Matched); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if personID > 0 {
				match.PersonID = new(personID)
				curated = true
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
			if match.PersonID != nil {
				matches.basis = append(matches.basis, match)
				matches.people[*match.PersonID] = true
				continue
			}
			component, err := resolve(*match.ParticipantID)
			if err != nil {
				return nil, err
			}
			if len(component.people) == 0 {
				// Curated ownership suppresses only redundant unbound evidence
				// on this contact. Bound people and other contacts still count.
				if !curated {
					matches.basis = append(matches.basis, match)
					matches.participants[component.canonical] = true
				}
			} else {
				for _, id := range component.people {
					bound := match
					bound.PersonID = new(id)
					matches.basis = append(matches.basis, bound)
					matches.people[id] = true
				}
			}
		}
	}
	return matches, nil
}

func (s *Store) resolvePersonCreateMatchesTx(ctx context.Context, tx *loggedTx, points []personCreateContactPoint, matches *personCreateMatches) (*Person, error) {
	people := slices.Sorted(maps.Keys(matches.people))
	participants := slices.Sorted(maps.Keys(matches.participants))
	if len(people) == 1 && len(participants) == 0 {
		person, err := s.getPersonTx(ctx, tx, people[0])
		if err != nil {
			return nil, err
		}
		info := &PersonEnsureInfo{Status: PersonEnsureMatchedExisting, MatchBasis: matches.basis, UnappliedContacts: []PersonUnappliedContact{}}
		for _, point := range points {
			matched := false
			for _, match := range matches.basis {
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
		id := participants[0]
		return nil, &PersonMatchesParticipantError{ParticipantID: id, MatchBasis: matches.basis, Next: &PersonCreateNext{CLI: []string{"msgvault", "person", "promote", strconv.FormatInt(id, 10), "--json"}, MCP: PersonCreateMCPNext{Tool: "promote_person", Arguments: map[string]int64{"participant_id": id}}}}
	}
	candidates := []PersonCreateCandidate{}
	for _, group := range []struct {
		ids   []int64
		table string
	}{{people, "persons"}, {participants, "participants"}} {
		for _, id := range group.ids {
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
	return nil, &PersonAmbiguousError{Candidates: candidates, MatchBasis: matches.basis}
}
