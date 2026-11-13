package store

import (
	"context"
	"sort"
	"strings"

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

// introductionIdentitySQL is shared by preflight and delegated list eligibility.
func (s *Store) introductionIdentitySQL(personExpr, typeExpr, sourceExpr, accountExpr string) string {
	user := beeperidentity.ScopedUserSQL(s.dialect.DriverName(), accountExpr, "pi.identifier_value")
	types := []string{}
	// Keep the source-type contract with SourceTypeUsesEmailIdentity.
	for _, typ := range emailIdentitySourceTypes {
		types = append(types, "'"+typ+"'")
	}
	return `SELECT pp.participant_id, ` + user + ` AS user_id FROM person_participants pp JOIN participant_identifiers pi ON pi.participant_id = pp.participant_id AND pi.identifier_type = 'beeper' WHERE pp.person_id = ` + personExpr + ` AND ` + typeExpr + ` = 'beeper' AND ` + user + ` IS NOT NULL
 UNION
 SELECT pp.participant_id, LOWER(TRIM(mr.email_address)) AS user_id
 FROM person_participants pp JOIN message_recipients mr ON mr.participant_id = pp.participant_id
 JOIN messages m ON m.id = mr.message_id JOIN sources src ON src.id = m.source_id
 JOIN conversations c ON c.id = m.conversation_id
 WHERE pp.person_id = ` + personExpr + ` AND ` + typeExpr + ` IN (` + strings.Join(types, ",") + `)
 AND m.source_id = ` + sourceExpr + ` AND c.conversation_type = 'email_thread'
 AND mr.recipient_type IN ('from','to','cc','bcc') AND COALESCE(TRIM(mr.email_address), '') <> ''
 AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL AND NOT ` + s.activityUnsentSQL()
}

func (s *Store) introductionPreflightContext(ctx context.Context, sourceID, personAID, personBID int64, owners loopOwners) (PersonIntroductionPreflight, error) {
	out := PersonIntroductionPreflight{Members: []IntroductionMember{}, Blockers: []IntroductionBlocker{}}
	source, err := s.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return out, err
	}
	if source.SourceType != sourceTypeBeeper && !SourceTypeUsesEmailIdentity(source.SourceType) {
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
		rows, err := s.db.QueryContext(ctx, `WITH input AS (SELECT CAST(? AS BIGINT) AS person_id, CAST(? AS TEXT) AS account, CAST(? AS TEXT) AS source_type, CAST(? AS BIGINT) AS source_id) `+s.introductionIdentitySQL("(SELECT person_id FROM input)", "(SELECT source_type FROM input)", "(SELECT source_id FROM input)", "(SELECT account FROM input)"), personID, source.Identifier, source.SourceType, source.ID)
		if err != nil {
			return out, err
		}
		var candidates []IntroductionMember
		for rows.Next() {
			var participant int64
			var user string
			if err := rows.Scan(&participant, &user); err != nil {
				_ = rows.Close()
				return out, err
			}
			candidates = append(candidates, IntroductionMember{personID, participant, user})
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
			out.Members = append(out.Members, candidates[0])
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
