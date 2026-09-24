package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"slices"
)

var ErrIdentityMatchReviewStale = errors.New("identity match review snapshot is stale")

// identityMatchReviewQuerier is shared by ordinary reads and the transaction
// holding the identity mutation lock. The same fingerprint code runs in both.
type identityMatchReviewQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*loggedRows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) GetIdentityMatchReviewContext(
	ctx context.Context, candidateID int64,
) (*IdentityMatchCandidate, error) {
	var result *IdentityMatchCandidate
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		candidate, err := getIdentityMatchCandidateTx(ctx, tx, candidateID)
		if err != nil {
			return err
		}
		if err := populateIdentityMatchReviewContext(ctx, tx, candidate); err != nil {
			return err
		}
		result = candidate
		return nil
	})
	return result, err
}

func (s *Store) ListIdentityMatchReviewsContext(
	ctx context.Context, states []IdentityMatchState, limit, offset int,
) ([]IdentityMatchCandidate, error) {
	candidates, err := s.ListIdentityMatchCandidatesContext(ctx, states, limit, offset)
	if err != nil {
		return nil, err
	}
	result := make([]IdentityMatchCandidate, 0, len(candidates))
	for i := range candidates {
		current, err := s.GetIdentityMatchReviewContext(ctx, candidates[i].ID)
		if errors.Is(err, ErrIdentityMatchNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(states) > 0 {
			if !slices.Contains(states, current.State) {
				continue
			}
		}
		result = append(result, *current)
	}
	return result, nil
}

func populateIdentityMatchReviewContext(
	ctx context.Context, q identityMatchReviewQuerier, candidate *IdentityMatchCandidate,
) error {
	if err := loadIdentityMatchSourceSupportContext(ctx, q, candidate); err != nil {
		return err
	}
	var err error
	candidate.ApplicationPending = candidate.State == IdentityMatchStateAccepted && candidate.applicationPending
	candidate.Actionable = candidate.State == IdentityMatchStateCandidate ||
		candidate.State == IdentityMatchStateConflict
	switch {
	case candidate.LeftKind == IdentityMatchParticipant && candidate.RightKind == IdentityMatchParticipant:
		candidate.LeftPerson, err = identityMatchClusterPersonContext(ctx, q, candidate.LeftID)
		if err != nil {
			return err
		}
		candidate.RightPerson, err = identityMatchClusterPersonContext(ctx, q, candidate.RightID)
		if err != nil {
			return err
		}
		if candidate.LeftPerson != nil && candidate.RightPerson != nil &&
			candidate.LeftPerson.PersonID != candidate.RightPerson.PersonID {
			candidate.Actionable = false
			candidate.Blocker = "person_merge_required"
		}
	case candidate.LeftKind == IdentityMatchCardDAVResource && candidate.RightKind == IdentityMatchPerson:
		// CardDAV resource acceptance has its own application path.
	default:
		candidate.Actionable = false
		candidate.Blocker = "endpoint_unsupported"
	}
	token, err := identityMatchFingerprintContext(ctx, q, candidate, true)
	if err != nil {
		return err
	}
	candidate.ReviewToken = token
	return nil
}

const identityMatchClusterBindingsSQL = `WITH RECURSIVE component(id) AS (
	SELECT CAST(? AS BIGINT) UNION
	SELECT CASE WHEN l.participant_a = component.id THEN l.participant_b ELSE l.participant_a END
	FROM participant_links l JOIN component ON l.participant_a = component.id OR l.participant_b = component.id
)
SELECT DISTINCT p.id, p.revision, EXISTS (
	SELECT 1 FROM carddav_publications cp WHERE cp.person_id = p.id
		AND (cp.desired = TRUE OR cp.pending_operation IS NOT NULL)
) FROM person_participants pp
JOIN persons p ON p.id = pp.person_id
WHERE pp.participant_id IN (SELECT id FROM component) ORDER BY p.id`

const identityMatchClusterPublicationsSQL = `WITH RECURSIVE component(id) AS (
	SELECT CAST(? AS BIGINT) UNION
	SELECT CASE WHEN l.participant_a = component.id THEN l.participant_b ELSE l.participant_a END
	FROM participant_links l JOIN component ON l.participant_a = component.id OR l.participant_b = component.id
)
SELECT cp.* FROM carddav_publications cp JOIN person_participants pp ON pp.person_id = cp.person_id
WHERE pp.participant_id IN (SELECT id FROM component) ORDER BY cp.person_id`

func identityMatchClusterPersonContext(ctx context.Context, q identityMatchReviewQuerier, participantID int64) (*IdentityMatchPersonBinding, error) {
	rows, err := q.QueryContext(ctx, identityMatchClusterBindingsSQL, participantID)
	if err != nil {
		return nil, fmt.Errorf("read match cluster binding: %w", err)
	}
	var binding *IdentityMatchPersonBinding
	for rows.Next() {
		var current IdentityMatchPersonBinding
		if err := rows.Scan(&current.PersonID, &current.Revision, &current.ActiveCardDAVPublication); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if binding != nil {
			_ = rows.Close()
			return nil, errors.New("identity cluster has multiple person bindings")
		}
		binding = &current
	}
	err = rows.Err()
	_ = rows.Close()
	return binding, err
}

func loadIdentityMatchSourceSupportContext(
	ctx context.Context, q identityMatchReviewQuerier, candidate *IdentityMatchCandidate,
) error {
	rows, err := q.QueryContext(ctx, `SELECT source_id, is_conservative
		FROM identity_match_candidate_sources WHERE candidate_id = ? ORDER BY source_id`, candidate.ID)
	if err != nil {
		return fmt.Errorf("load match candidate source support: %w", err)
	}
	candidate.SourceSupport = []IdentityMatchSourceSupport{}
	for rows.Next() {
		var support IdentityMatchSourceSupport
		if err := rows.Scan(&support.SourceID, &support.IsConservative); err != nil {
			_ = rows.Close()
			return err
		}
		candidate.SourceSupport = append(candidate.SourceSupport, support)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT es.evidence_id, es.source_id, es.is_conservative
		FROM identity_match_evidence_sources es
		JOIN identity_match_evidence e ON e.id = es.evidence_id
		WHERE e.candidate_id = ? ORDER BY es.evidence_id, es.source_id`, candidate.ID)
	if err != nil {
		return fmt.Errorf("load match evidence source support: %w", err)
	}
	byEvidence := make(map[int64][]IdentityMatchSourceSupport)
	for rows.Next() {
		var evidenceID int64
		var support IdentityMatchSourceSupport
		if err := rows.Scan(&evidenceID, &support.SourceID, &support.IsConservative); err != nil {
			_ = rows.Close()
			return err
		}
		byEvidence[evidenceID] = append(byEvidence[evidenceID], support)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for i := range candidate.Evidence {
		candidate.Evidence[i].SourceSupport = byEvidence[candidate.Evidence[i].ID]
	}
	return nil
}

// The review token includes decision state. The application fingerprint does
// not: an accepted decision necessarily changes the candidate's state, but
// its evidence and endpoint context must remain the same until link creation.
func identityMatchFingerprintContext(
	ctx context.Context, q identityMatchReviewQuerier,
	c *IdentityMatchCandidate, includeDecision bool,
) (string, error) {
	h := sha256.New()
	base := []any{c.ID, c.LeftKind, c.LeftID, c.RightKind, c.RightID,
		c.Basis, c.ServiceSlug, c.ScopeKind, c.ScopeValue, c.NormalizedValue,
		c.Confidence, c.Source, c.SourceRef}
	if includeDecision {
		base = append(base, c.State, c.DecidedBy, c.DecidedAt, c.Notes,
			c.applicationPending,
			c.conflictState.observationOrigin.String, c.conflictState.observationOrigin.Valid,
			c.conflictState.preConflictState.String, c.conflictState.preConflictState.Valid)
	}
	if err := hashIdentityMatchValue(h, base); err != nil {
		return "", err
	}
	queries := []struct {
		name, sql string
		args      []any
	}{
		{"candidate_sources", `SELECT * FROM identity_match_candidate_sources WHERE candidate_id = ? ORDER BY source_id`, []any{c.ID}},
		{"evidence", `SELECT * FROM identity_match_evidence WHERE candidate_id = ? ORDER BY id`, []any{c.ID}},
		{"evidence_sources", `SELECT es.* FROM identity_match_evidence_sources es JOIN identity_match_evidence e ON e.id = es.evidence_id WHERE e.candidate_id = ? ORDER BY es.evidence_id, es.source_id`, []any{c.ID}},
	}
	for _, endpoint := range []struct {
		kind IdentityMatchEndpointKind
		id   int64
	}{{c.LeftKind, c.LeftID}, {c.RightKind, c.RightID}} {
		switch endpoint.kind {
		case IdentityMatchParticipant:
			queries = append(queries,
				struct {
					name, sql string
					args      []any
				}{"participant", `SELECT * FROM participants WHERE id = ?`, []any{endpoint.id}},
				struct {
					name, sql string
					args      []any
				}{"identifiers", `SELECT * FROM participant_identifiers WHERE participant_id = ? ORDER BY id`, []any{endpoint.id}},
				struct {
					name, sql string
					args      []any
				}{"observations", `SELECT * FROM participant_contact_observations WHERE participant_id = ? ORDER BY id`, []any{endpoint.id}},
				struct {
					name, sql string
					args      []any
				}{"bindings", identityMatchClusterBindingsSQL, []any{endpoint.id}},
				struct {
					name, sql string
					args      []any
				}{"bound_publications", identityMatchClusterPublicationsSQL, []any{endpoint.id}},
			)
		case IdentityMatchPerson:
			queries = append(queries,
				struct {
					name, sql string
					args      []any
				}{"person", `SELECT * FROM persons WHERE id = ?`, []any{endpoint.id}},
				struct {
					name, sql string
					args      []any
				}{"publication", `SELECT person_id, desired, address_book_id, href, remote_etag, mapping_revision, mutation_revision, approved_body_sha256 FROM carddav_publications WHERE person_id = ?`, []any{endpoint.id}},
			)
		case IdentityMatchCardDAVResource:
			queries = append(queries, struct {
				name, sql string
				args      []any
			}{"carddav_resource", `SELECT id, address_book_id, href, remote_uid, remote_etag, remote_semantic_hash, mapping_status, mapping_revision, person_id, person_revision_at_bind FROM carddav_resources WHERE id = ?`, []any{endpoint.id}})
		case IdentityMatchObservation, IdentityMatchContactPoint:
			// These endpoint kinds have no materialized identity rows.
		}
	}
	for _, query := range queries {
		if err := hashIdentityMatchValue(h, query.name); err != nil {
			return "", err
		}
		rows, err := q.QueryContext(ctx, query.sql, query.args...)
		if err != nil {
			return "", fmt.Errorf("fingerprint identity match %s: %w", query.name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return "", err
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				return "", err
			}
			if err := hashIdentityMatchValue(h, values); err != nil {
				_ = rows.Close()
				return "", err
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashIdentityMatchValue(h hash.Hash, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = h.Write(append(data, '\n'))
	return err
}

// DecideIdentityMatchReviewedContext checks exactly what the caller reviewed
// while holding the identity mutation lock, then uses the existing guarded
// application path. A rejection retains the suggestion without linking it.
func (s *Store) DecideIdentityMatchReviewedContext(
	ctx context.Context, candidateID int64, token string,
	decision IdentityMatchState, notes *string,
) (*IdentityMatchCandidate, int64, error) {
	if token == "" {
		return nil, 0, ErrIdentityMatchReviewStale
	}
	if decision != IdentityMatchStateAccepted && decision != IdentityMatchStateRejected {
		return nil, 0, ErrInvalidIdentityMatchState
	}
	if decision == IdentityMatchStateAccepted {
		candidate, err := s.GetIdentityMatchCandidateContext(ctx, candidateID)
		if err != nil {
			return nil, 0, err
		}
		if candidate.LeftKind == IdentityMatchCardDAVResource &&
			candidate.RightKind == IdentityMatchPerson {
			return s.acceptCardDAVIdentityMatchCandidateReviewedContext(
				ctx, candidateID, string(ProvenanceUser), notes, &token)
		}
		if candidate.LeftKind != IdentityMatchParticipant || candidate.RightKind != IdentityMatchParticipant {
			return nil, 0, ErrIdentityMatchEndpointUnsupported
		}
	}
	candidate, before, err := s.decideIdentityMatchCandidateContext(
		ctx, candidateID, decision, string(ProvenanceUser), notes, &token)
	if err != nil {
		return nil, 0, err
	}
	if decision == IdentityMatchStateRejected || !candidate.applicationPending {
		revision, err := readIdentityRevisionContext(ctx, s.db)
		return candidate, revision, err
	}
	if s.identityMatchReviewAfterDecisionHook != nil {
		s.identityMatchReviewAfterDecisionHook()
	}
	applied, revision, _, err := s.applyAcceptedIdentityMatchCandidateContext(
		ctx, candidate, string(ProvenanceUser))
	if err != nil {
		if errors.Is(err, ErrPersonBindingConflict) && before.State != IdentityMatchStateAccepted {
			if restoreErr := s.restoreIdentityMatchDecisionAfterBindingConflictContext(
				ctx, before, candidate); restoreErr != nil {
				return nil, 0, errors.Join(err, restoreErr)
			}
		}
		return nil, 0, err
	}
	return applied, revision, nil
}
