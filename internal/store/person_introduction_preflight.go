package store

import (
	"context"
	"fmt"
	"sort"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

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

func (s *Store) GetPersonIntroductionPreflightContext(ctx context.Context, sourceID, personAID, personBID int64) (PersonIntroductionPreflight, error) {
	out := PersonIntroductionPreflight{Members: []IntroductionMember{}, Blockers: []IntroductionBlocker{}}
	source, err := s.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return out, err
	}
	if source.SourceType != "beeper" {
		return out, ErrIntroductionSource
	}
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pi.participant_id FROM account_identities ai JOIN sources src ON src.id = ai.source_id JOIN participant_identifiers pi ON pi.identifier_type = 'beeper' AND pi.identifier_value = `+beeperidentity.FallbackSQL(s.dialect.DriverName(), "src.identifier", "ai.address")+` WHERE ai.source_id = ?`, sourceID)
	if err != nil {
		return out, fmt.Errorf("load scoped owner identities: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return out, err
		}
		owners.participants[id] = struct{}{}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return out, err
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
