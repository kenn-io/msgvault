package store

import (
	"context"
	"sort"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// IntroductionPeopleVisibleContext checks account-scoped identities without
// returning person-dependent details. Owners are excluded across all sources.
func (s *Store) IntroductionPeopleVisibleContext(ctx context.Context, account string, personIDs ...int64) (bool, error) {
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return false, err
	}
	for _, personID := range personIDs {
		if _, owner := owners.persons[personID]; owner {
			return false, nil
		}
		rows, err := s.db.QueryContext(ctx, `SELECT pi.identifier_value FROM person_participants pp JOIN participant_identifiers pi ON pi.participant_id = pp.participant_id AND pi.identifier_type = 'beeper' WHERE pp.person_id = ?`, personID)
		if err != nil {
			return false, err
		}
		visible := false
		for rows.Next() {
			var identifier string
			if err := rows.Scan(&identifier); err != nil {
				_ = rows.Close()
				return false, err
			}
			if _, ok := beeperidentity.UserID(account, identifier); ok {
				visible = true
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return false, err
		}
		if !visible {
			return false, nil
		}
	}
	return true, nil
}

type IntroductionMember struct {
	PersonID      int64  `json:"person_id"`
	ParticipantID int64  `json:"participant_id"`
	UserID        string `json:"user_id"`
}
type IntroductionBlocker struct {
	Code     string `json:"code"`
	PersonID int64  `json:"person_id"`
}
type PersonIntroductionPreflight struct {
	Members  []IntroductionMember  `json:"members"`
	Blockers []IntroductionBlocker `json:"blockers"`
}

// GetPersonIntroductionPreflightsContext shares the global owner lookup while
// retaining account-scoped checks for each introduction.
func (s *Store) GetPersonIntroductionPreflightsContext(ctx context.Context, inputs []PersonIntroductionPrepare) ([]PersonIntroductionPreflight, error) {
	results := make([]PersonIntroductionPreflight, 0, len(inputs))
	if len(inputs) == 0 {
		return results, nil
	}
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return nil, err
	}
	for _, in := range inputs {
		result, err := s.introductionPreflightContext(ctx, in.SourceID, in.PersonAID, in.PersonBID, owners)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Store) GetPersonIntroductionPreflightContext(ctx context.Context, sourceID, personAID, personBID int64) (PersonIntroductionPreflight, error) {
	results, err := s.GetPersonIntroductionPreflightsContext(ctx, []PersonIntroductionPrepare{{SourceID: sourceID, PersonAID: personAID, PersonBID: personBID}})
	if err != nil {
		return PersonIntroductionPreflight{}, err
	}
	return results[0], nil
}

func (s *Store) introductionPreflightContext(ctx context.Context, sourceID, personAID, personBID int64, owners loopOwners) (PersonIntroductionPreflight, error) {
	out := PersonIntroductionPreflight{Members: []IntroductionMember{}, Blockers: []IntroductionBlocker{}}
	source, err := s.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return out, err
	}
	if source.SourceType != sourceTypeBeeper {
		return out, ErrIntroductionSource
	}
	if source.MergedIntoSourceID != 0 {
		out.Blockers = append(out.Blockers, IntroductionBlocker{Code: "source_retired"})
	}
	for i, personID := range []int64{personAID, personBID} {
		if personAID == personBID {
			if i > 0 {
				break
			}
			out.Blockers = append(out.Blockers, IntroductionBlocker{"same_person", personID})
		}
		_, owner := owners.persons[personID]
		rows, err := s.db.QueryContext(ctx, `SELECT pp.participant_id, pi.identifier_value FROM person_participants pp LEFT JOIN participant_identifiers pi ON pi.participant_id = pp.participant_id AND pi.identifier_type = 'beeper' WHERE pp.person_id = ? ORDER BY pp.participant_id, pi.identifier_value`, personID)
		if err != nil {
			return out, err
		}
		candidates := map[IntroductionMember]struct{}{}
		for rows.Next() {
			var participant int64
			var key *string
			if err := rows.Scan(&participant, &key); err != nil {
				_ = rows.Close()
				return out, err
			}
			if _, ok := owners.participants[participant]; ok {
				owner = true
			}
			if key != nil {
				if user, ok := beeperidentity.UserID(source.Identifier, *key); ok {
					candidates[IntroductionMember{personID, participant, user}] = struct{}{}
				}
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return out, err
		}
		if owner {
			out.Blockers = append(out.Blockers, IntroductionBlocker{"owner_selected", personID})
		}
		switch len(candidates) {
		case 0:
			out.Blockers = append(out.Blockers, IntroductionBlocker{"no_identity_on_account", personID})
		case 1:
			for member := range candidates {
				out.Members = append(out.Members, member)
			}
		default:
			out.Blockers = append(out.Blockers, IntroductionBlocker{"ambiguous_identity", personID})
		}
	}
	sort.Slice(out.Blockers, func(i, j int) bool {
		if out.Blockers[i].PersonID != out.Blockers[j].PersonID {
			return out.Blockers[i].PersonID < out.Blockers[j].PersonID
		}
		return out.Blockers[i].Code < out.Blockers[j].Code
	})
	return out, nil
}
