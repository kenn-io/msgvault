package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// personHasUserOwnedStateTx reports whether deleting personID would discard
// state that is user-authored, user-reviewed, or changed since the supplied
// baseline revision. Import cleanup calls this before removing identity-match
// candidates because their decisions and evidence are part of the contract.
func (s *Store) personHasUserOwnedStateTx(
	ctx context.Context, tx *loggedTx, personID, baselineRevision int64,
) (bool, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`+
		s.dialect.SelectForUpdate(), personID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock person %d user-owned state: %w", personID, err)
	}
	if revision != baselineRevision {
		return true, nil
	}
	var hasUserOwnedState bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM persons
		WHERE id = ? AND (
			EXISTS (SELECT 1 FROM person_tracking
				WHERE person_id = persons.id)
			OR EXISTS (SELECT 1 FROM carddav_publications
				WHERE person_id = persons.id)
			OR EXISTS (SELECT 1 FROM person_participants
				WHERE person_id = persons.id)
			OR EXISTS (SELECT 1 FROM person_names
				WHERE person_id = persons.id AND source = 'user')
			OR EXISTS (SELECT 1 FROM person_contact_points
				WHERE person_id = persons.id AND source = 'user')
			OR EXISTS (SELECT 1 FROM person_addresses
				WHERE person_id = persons.id AND source = 'user')
			OR EXISTS (SELECT 1 FROM person_dates
				WHERE person_id = persons.id AND source = 'user')
			OR EXISTS (SELECT 1 FROM person_categories
				WHERE person_id = persons.id AND source = 'user')
			OR EXISTS (SELECT 1 FROM person_media
				WHERE person_id = persons.id AND source = 'user')
			OR EXISTS (SELECT 1 FROM person_relationships
				WHERE source_person_id = persons.id OR target_person_id = persons.id)
			OR EXISTS (SELECT 1 FROM person_relationship_reviews
				WHERE (person_id = persons.id OR matched_person_id = persons.id)
				  AND (status IN ('accepted', 'rejected')
				    OR reviewed_by IS NOT NULL OR reviewed_at IS NOT NULL))
			OR EXISTS (SELECT 1 FROM employments
				WHERE source = 'user' AND person_id = persons.id)
			OR EXISTS (SELECT 1 FROM person_attribute_values
				WHERE source = 'user' AND person_id = persons.id)
			OR EXISTS (SELECT 1 FROM daily_note_entry_persons
				WHERE person_id = persons.id)
			OR EXISTS (
				SELECT 1 FROM identity_match_candidates candidate
				WHERE (
					(candidate.left_kind = 'person' AND candidate.left_id = persons.id)
					OR (candidate.right_kind = 'person' AND candidate.right_id = persons.id)
					OR (candidate.left_kind = 'contact_point' AND EXISTS (
						SELECT 1 FROM person_contact_points point
						WHERE point.id = candidate.left_id AND point.person_id = persons.id
					))
					OR (candidate.right_kind = 'contact_point' AND EXISTS (
						SELECT 1 FROM person_contact_points point
						WHERE point.id = candidate.right_id AND point.person_id = persons.id
					))
				) AND (
					candidate.state IN ('accepted', 'rejected', 'conflict')
					OR candidate.decided_by IS NOT NULL
					OR candidate.decided_at IS NOT NULL
					OR candidate.notes IS NOT NULL
					OR candidate.source = 'user'
					OR EXISTS (SELECT 1 FROM identity_match_evidence evidence
						WHERE evidence.candidate_id = candidate.id AND evidence.source = 'user')
				)
			)
		)
	)`, personID).Scan(&hasUserOwnedState)
	if err != nil {
		return false, fmt.Errorf("check person %d user-owned state: %w", personID, err)
	}
	return hasUserOwnedState, nil
}
