package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Addresses seen on more participants than this are too broad to seed useful
// pairwise reviews and would otherwise create quadratic work before LIMIT.
const personMatchBlockingMaxParticipantsPerEmail = 20

// EnsurePersonMatchScoringCandidatesContext makes a bounded, indexed pass over
// current email observations to find participant pairs missed by an importer.
// It only adds review candidates and their source evidence; it never links.
func (s *Store) EnsurePersonMatchScoringCandidatesContext(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("person match blocking limit must be 1-100")
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`WITH RECURSIVE shared_email_values AS (
		SELECT normalized_value
		FROM participant_contact_observations
		WHERE address_kind = 'email' AND normalized_value <> ''
		  AND source_id IS NOT NULL AND source = 'archive_observation'
		  AND active_until IS NULL AND superseded_at IS NULL
		GROUP BY normalized_value
		HAVING COUNT(DISTINCT participant_id) BETWEEN 2 AND ?
	), email_pairs AS (
		SELECT a.participant_id AS left_id, b.participant_id AS right_id,
			a.normalized_value, a.source_id AS left_source, b.source_id AS right_source
		FROM participant_contact_observations a
		JOIN participant_contact_observations b
		  ON b.address_kind = a.address_kind AND b.normalized_value = a.normalized_value
		 AND b.participant_id > a.participant_id
		WHERE a.address_kind = 'email' AND a.normalized_value <> ''
		  AND a.source_id IS NOT NULL AND b.source_id IS NOT NULL
		  AND a.source = 'archive_observation' AND b.source = 'archive_observation'
		  AND a.active_until IS NULL AND a.superseded_at IS NULL
		  AND b.active_until IS NULL AND b.superseded_at IS NULL
		  AND EXISTS (SELECT 1 FROM shared_email_values v WHERE v.normalized_value = a.normalized_value)
	), reachable(left_id, right_id, participant_id) AS (
		SELECT left_id, right_id, left_id FROM email_pairs
		UNION
		SELECT r.left_id, r.right_id,
			CASE WHEN links.participant_a = r.participant_id
				THEN links.participant_b ELSE links.participant_a END
		FROM reachable r
		JOIN participant_links links
		  ON links.participant_a = r.participant_id OR links.participant_b = r.participant_id
	), eligible_pairs AS (
		SELECT pairs.left_id, pairs.right_id, pairs.normalized_value
		FROM email_pairs pairs
		WHERE NOT EXISTS (SELECT 1 FROM reachable r
			WHERE r.left_id = pairs.left_id AND r.right_id = pairs.right_id
			  AND r.participant_id = pairs.right_id)
		GROUP BY pairs.left_id, pairs.right_id, pairs.normalized_value
	), pending_pairs AS (
		SELECT pairs.left_id, pairs.right_id, pairs.normalized_value
		FROM eligible_pairs pairs
		WHERE NOT EXISTS (SELECT 1 FROM identity_match_candidates c
			WHERE c.left_kind = 'participant' AND c.right_kind = 'participant'
			  AND c.left_id = pairs.left_id AND c.right_id = pairs.right_id
			  AND c.basis = 'email' AND c.normalized_value = pairs.normalized_value)
		OR EXISTS (SELECT 1 FROM email_pairs ep
			JOIN identity_match_candidates c
			  ON c.left_kind = 'participant' AND c.right_kind = 'participant'
			 AND c.left_id = ep.left_id AND c.right_id = ep.right_id
			 AND c.basis = 'email' AND c.normalized_value = ep.normalized_value
			WHERE ep.left_id = pairs.left_id AND ep.right_id = pairs.right_id
			  AND ep.normalized_value = pairs.normalized_value
			  AND (
				NOT EXISTS (SELECT 1 FROM identity_match_candidate_sources cs
					WHERE cs.candidate_id = c.id AND cs.source_id = ep.left_source)
				OR NOT EXISTS (SELECT 1 FROM identity_match_candidate_sources cs
					WHERE cs.candidate_id = c.id AND cs.source_id = ep.right_source)
				OR NOT EXISTS (SELECT 1 FROM identity_match_evidence e
					JOIN identity_match_evidence_sources es ON es.evidence_id = e.id
					WHERE e.candidate_id = c.id AND e.evidence_kind = 'email'
					  AND e.detail = ep.normalized_value AND e.source = 'archive_observation'
					  AND es.source_id = ep.left_source)
				OR NOT EXISTS (SELECT 1 FROM identity_match_evidence e
					JOIN identity_match_evidence_sources es ON es.evidence_id = e.id
					WHERE e.candidate_id = c.id AND e.evidence_kind = 'email'
					  AND e.detail = ep.normalized_value AND e.source = 'archive_observation'
					  AND es.source_id = ep.right_source)
			  ))
	)
	SELECT left_id, right_id, normalized_value FROM pending_pairs
	ORDER BY normalized_value, left_id, right_id LIMIT ?`), personMatchBlockingMaxParticipantsPerEmail, limit)
	if err != nil {
		return 0, fmt.Errorf("find missing person match pairs: %w", err)
	}
	type pair struct {
		left, right int64
		value       string
	}
	pairs := make([]pair, 0, limit)
	for rows.Next() {
		var item pair
		if err := rows.Scan(&item.left, &item.right, &item.value); err != nil {
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
		candidateCreated := false
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
			if err != nil {
				return err
			}
			candidateCreated = wasCreated
			observations, err := tx.QueryContext(ctx, `SELECT participant_id, normalized_value, source_id
				FROM participant_contact_observations
				WHERE participant_id IN (?, ?) AND address_kind = 'email' AND normalized_value = ?
				  AND source_id IS NOT NULL AND source = 'archive_observation'
				  AND active_until IS NULL AND superseded_at IS NULL
				ORDER BY participant_id, source_id`, item.left, item.right, item.value)
			if err != nil {
				return fmt.Errorf("load person match email observations: %w", err)
			}
			type emailSupport struct{ left, right map[int64]struct{} }
			support := make(map[string]*emailSupport)
			for observations.Next() {
				var participantID, sourceID int64
				var value string
				if err := observations.Scan(&participantID, &value, &sourceID); err != nil {
					_ = observations.Close()
					return err
				}
				entry := support[value]
				if entry == nil {
					entry = &emailSupport{left: make(map[int64]struct{}), right: make(map[int64]struct{})}
					support[value] = entry
				}
				if participantID == item.left {
					entry.left[sourceID] = struct{}{}
				} else {
					entry.right[sourceID] = struct{}{}
				}
			}
			if err := observations.Err(); err != nil {
				_ = observations.Close()
				return err
			}
			_ = observations.Close()
			for value, entry := range support {
				if len(entry.left) == 0 || len(entry.right) == 0 {
					continue
				}
				var evidenceID int64
				err := tx.QueryRowContext(ctx, `SELECT id FROM identity_match_evidence
					WHERE candidate_id = ? AND evidence_kind = 'email' AND detail = ? AND source = ?
					ORDER BY id LIMIT 1`, candidate.ID, value, ProvenanceArchiveObservation).Scan(&evidenceID)
				if errors.Is(err, sql.ErrNoRows) {
					err = tx.QueryRowContext(ctx, `INSERT INTO identity_match_evidence
						(candidate_id, evidence_kind, detail, source) VALUES (?, 'email', ?, ?)
						RETURNING id`, candidate.ID, value, ProvenanceArchiveObservation).Scan(&evidenceID)
				}
				if err != nil {
					return fmt.Errorf("seed person match evidence: %w", err)
				}
				for sourceID := range entry.left {
					entry.right[sourceID] = struct{}{}
				}
				for sourceID := range entry.right {
					if err := s.recordIdentityMatchCandidateSourceTx(ctx, tx, candidate.ID, &sourceID); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO identity_match_evidence_sources
						(evidence_id, source_id, is_conservative) VALUES (?, ?, FALSE)
						ON CONFLICT (evidence_id, source_id) DO UPDATE SET is_conservative = FALSE`, evidenceID, sourceID); err != nil {
						return fmt.Errorf("seed person match evidence source: %w", err)
					}
				}
			}
			return nil
		})
		if err != nil {
			return created, err
		}
		if candidateCreated {
			created++
		}
	}
	return created, nil
}
