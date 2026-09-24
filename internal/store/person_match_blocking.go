package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EnsurePersonMatchScoringCandidatesContext makes a bounded, indexed pass over
// current email observations to find participant pairs missed by an importer.
// It only adds review candidates and their source evidence; it never links.
func (s *Store) EnsurePersonMatchScoringCandidatesContext(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("person match blocking limit must be 1-100")
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`WITH RECURSIVE email_pairs AS (
		SELECT a.participant_id AS left_id, b.participant_id AS right_id,
			a.normalized_value, a.source_id AS left_source, b.source_id AS right_source
		FROM participant_contact_observations a
		JOIN participant_contact_observations b
		  ON b.address_kind = a.address_kind AND b.normalized_value = a.normalized_value
		 AND b.participant_id > a.participant_id
		WHERE a.address_kind = 'email' AND a.normalized_value <> ''
		  AND a.active_until IS NULL AND a.superseded_at IS NULL
		  AND b.active_until IS NULL AND b.superseded_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM identity_match_candidates c
		    WHERE c.left_kind = 'participant' AND c.right_kind = 'participant'
		      AND c.left_id = a.participant_id AND c.right_id = b.participant_id)
	), reachable(left_id, right_id, participant_id) AS (
		SELECT left_id, right_id, left_id FROM email_pairs
		UNION
		SELECT r.left_id, r.right_id,
			CASE WHEN links.participant_a = r.participant_id
				THEN links.participant_b ELSE links.participant_a END
		FROM reachable r
		JOIN participant_links links
		  ON links.participant_a = r.participant_id OR links.participant_b = r.participant_id
	)
	SELECT pairs.left_id, pairs.right_id, pairs.normalized_value,
		pairs.left_source, pairs.right_source
	FROM email_pairs pairs
	WHERE NOT EXISTS (SELECT 1 FROM reachable r
		WHERE r.left_id = pairs.left_id AND r.right_id = pairs.right_id
		  AND r.participant_id = pairs.right_id)
	ORDER BY pairs.normalized_value, pairs.left_id, pairs.right_id LIMIT ?`), limit)
	if err != nil {
		return 0, fmt.Errorf("find missing person match pairs: %w", err)
	}
	type pair struct {
		left, right             int64
		value                   string
		leftSource, rightSource sql.NullInt64
	}
	pairs := make([]pair, 0, limit)
	for rows.Next() {
		var item pair
		if err := rows.Scan(&item.left, &item.right, &item.value, &item.leftSource, &item.rightSource); err != nil {
			_ = rows.Close()
			return 0, err
		}
		pairs = append(pairs, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	created := 0
	for _, item := range pairs {
		seeded := false
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
				return err
			}
			edges, err := s.loadLinkEdgesTxContext(ctx, tx)
			if err != nil {
				return err
			}
			if _, linked := componentOf(item.left, edges)[item.right]; linked {
				return nil
			}
			input := IdentityMatchCandidateInput{LeftKind: IdentityMatchParticipant, LeftID: item.left,
				RightKind: IdentityMatchParticipant, RightID: item.right,
				Basis: IdentityMatchEmail, NormalizedValue: &item.value,
				State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation}
			candidate, wasCreated, err := s.upsertIdentityMatchCandidateTx(ctx, tx, input,
				IdentityMatchParticipant, item.left, IdentityMatchParticipant, item.right, nil, false)
			if err != nil || !wasCreated {
				return err
			}
			var evidenceID int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO identity_match_evidence
				(candidate_id, evidence_kind, detail, source) VALUES (?, 'email', ?, ?)
				RETURNING id`, candidate.ID, item.value, ProvenanceArchiveObservation).Scan(&evidenceID); err != nil {
				return fmt.Errorf("seed person match evidence: %w", err)
			}
			seen := map[int64]bool{}
			for _, source := range []sql.NullInt64{item.leftSource, item.rightSource} {
				if !source.Valid || seen[source.Int64] {
					continue
				}
				seen[source.Int64] = true
				if err := s.recordIdentityMatchCandidateSourceTx(ctx, tx, candidate.ID, &source.Int64); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO identity_match_evidence_sources
					(evidence_id, source_id, is_conservative) VALUES (?, ?, FALSE)`, evidenceID, source.Int64); err != nil {
					return fmt.Errorf("seed person match evidence source: %w", err)
				}
			}
			seeded = true
			return nil
		})
		if err != nil {
			return created, err
		}
		if seeded {
			created++
		}
	}
	return created, nil
}
