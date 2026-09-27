package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AnchorIdentity is one email or E.164 phone that an importer asserts for a
// stable provider anchor.
type AnchorIdentity struct {
	Kind  ContactAddressKind // ContactAddressEmail or ContactAddressPhone
	Value string
}

// StableAnchorSettledContext reports whether linking the identities of one
// anchor would change nothing. It is true when every identity already exists
// as a participant carrying a current observation of the anchor from
// sourceID, and every other participant carrying the anchor has a decided
// stable_provider_id candidate with the first identity's participant: an
// applied acceptance, a rejection, or a conflict. It only reads, so importers
// can call it on every sync.
func (s *Store) StableAnchorSettledContext(
	ctx context.Context, sourceID int64, anchor string, identities []AnchorIdentity,
) (bool, error) {
	if anchor == "" || len(identities) == 0 {
		return true, nil
	}
	participantIDs := make([]int64, 0, len(identities))
	for _, identity := range identities {
		column := "email_address"
		if identity.Kind == ContactAddressPhone {
			column = "phone_number"
		}
		var id int64
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM participants WHERE `+column+` = ?`, identity.Value).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("look up anchored participant: %w", err)
		}
		var observed int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM participant_contact_observations
			WHERE participant_id = ? AND source_id = ? AND provider_user_id = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			id, sourceID, anchor).Scan(&observed); err != nil {
			return false, fmt.Errorf("check anchored observation: %w", err)
		}
		if observed == 0 {
			return false, nil
		}
		participantIDs = append(participantIDs, id)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT participant_id
		FROM participant_contact_observations
		WHERE provider_user_id = ? AND active_until IS NULL AND superseded_at IS NULL
		GROUP BY participant_id`, anchor)
	if err != nil {
		return false, fmt.Errorf("list anchored participants: %w", err)
	}
	var anchored []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return false, err
		}
		anchored = append(anchored, id)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}

	primary := participantIDs[0]
	for _, other := range anchored {
		if other == primary {
			continue
		}
		left, right := primary, other
		if left > right {
			left, right = right, left
		}
		var state IdentityMatchState
		var pending bool
		err := s.db.QueryRowContext(ctx, `SELECT state, application_pending
			FROM identity_match_candidates
			WHERE left_kind = ? AND left_id = ? AND right_kind = ? AND right_id = ?
			  AND basis = ? AND normalized_value = ?`,
			IdentityMatchParticipant, left, IdentityMatchParticipant, right,
			IdentityMatchStableProviderID, anchor).Scan(&state, &pending)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("check anchored candidate: %w", err)
		}
		switch {
		case state == IdentityMatchStateRejected, state == IdentityMatchStateConflict:
		case state == IdentityMatchStateAccepted && !pending:
		default:
			return false, nil
		}
	}
	return true, nil
}

// IsAccountIdentityAddressContext reports whether address is a confirmed
// account identity (one of the archive owner's own addresses) on any source.
func (s *Store) IsAccountIdentityAddressContext(ctx context.Context, address string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM account_identities
		WHERE LOWER(address) = LOWER(?) LIMIT 1`, address).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check account identity: %w", err)
	}
	return true, nil
}
