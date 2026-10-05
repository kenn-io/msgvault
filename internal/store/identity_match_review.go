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
	"strings"
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
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	args := make([]any, 0, len(states)+2)
	query := identityMatchCandidateSelect
	if len(states) > 0 {
		placeholders := make([]string, len(states))
		for i, state := range states {
			if !state.valid() {
				return nil, ErrInvalidIdentityMatchState
			}
			placeholders[i] = "?"
			args = append(args, state)
		}
		query += ` WHERE c.state IN (` + strings.Join(placeholders, ",") + `)`
	}
	query += ` ORDER BY c.id LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	result := make([]IdentityMatchCandidate, 0, limit)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list identity match reviews: %w", err)
		}
		for rows.Next() {
			candidate, scanErr := scanIdentityMatchCandidate(rows)
			if scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("scan identity match review: %w", scanErr)
			}
			result = append(result, *candidate)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("list identity match reviews: %w", err)
		}
		if err := loadIdentityMatchEvidencePageContext(ctx, tx, result); err != nil {
			return err
		}
		for i := range result {
			initializeIdentityMatchReviewActionable(&result[i])
		}
		if err := populateIdentityMatchReviewTokensContext(ctx, tx, result); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func loadIdentityMatchEvidencePageContext(
	ctx context.Context, q identityMatchReviewQuerier, candidates []IdentityMatchCandidate,
) error {
	if len(candidates) == 0 {
		return nil
	}
	placeholders := make([]string, len(candidates))
	args := make([]any, len(candidates))
	index := make(map[int64]int, len(candidates))
	for i := range candidates {
		placeholders[i] = "?"
		args[i] = candidates[i].ID
		index[candidates[i].ID] = i
	}
	rows, err := q.QueryContext(ctx, `SELECT
		id, candidate_id, evidence_kind, evidence_ref, detail, source, created_at
		FROM identity_match_evidence WHERE candidate_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY candidate_id, id`, args...)
	if err != nil {
		return fmt.Errorf("load identity match evidence page: %w", err)
	}
	evidence, err := scanIdentityMatchEvidenceRows(rows)
	if err != nil {
		return fmt.Errorf("load identity match evidence page: %w", err)
	}
	for _, item := range evidence {
		i := index[item.CandidateID]
		candidates[i].Evidence = append(candidates[i].Evidence, item)
	}
	return nil
}

func populateIdentityMatchReviewContext(
	ctx context.Context, q identityMatchReviewQuerier, candidate *IdentityMatchCandidate,
) error {
	if err := populateIdentityMatchReviewDetailsContext(ctx, q, candidate); err != nil {
		return err
	}
	token, err := identityMatchFingerprintContext(ctx, q, candidate, true)
	if err != nil {
		return err
	}
	candidate.ReviewToken = token
	return nil
}

func populateIdentityMatchReviewDetailsContext(
	ctx context.Context, q identityMatchReviewQuerier, candidate *IdentityMatchCandidate,
) error {
	if err := loadIdentityMatchSourceSupportContext(ctx, q, candidate); err != nil {
		return err
	}
	initializeIdentityMatchReviewActionable(candidate)
	var err error
	if candidate.LeftKind == IdentityMatchParticipant && candidate.RightKind == IdentityMatchParticipant {
		candidate.LeftPerson, err = identityMatchClusterPersonContext(ctx, q, candidate.LeftID)
		if err != nil {
			return err
		}
		candidate.RightPerson, err = identityMatchClusterPersonContext(ctx, q, candidate.RightID)
		if err != nil {
			return err
		}
	}
	applyIdentityMatchReviewBindingBlocker(candidate)
	return nil
}

func initializeIdentityMatchReviewActionable(candidate *IdentityMatchCandidate) {
	candidate.ApplicationPending = candidate.State == IdentityMatchStateAccepted && candidate.applicationPending
	candidate.Actionable = candidate.State == IdentityMatchStateCandidate ||
		candidate.State == IdentityMatchStateConflict
	switch {
	case candidate.LeftKind == IdentityMatchParticipant && candidate.RightKind == IdentityMatchParticipant:
	case candidate.LeftKind == IdentityMatchCardDAVResource && candidate.RightKind == IdentityMatchPerson:
		// CardDAV resource acceptance has its own application path.
	default:
		candidate.Actionable = false
		candidate.Blocker = "endpoint_unsupported"
	}
}

func applyIdentityMatchReviewBindingBlocker(candidate *IdentityMatchCandidate) {
	if candidate.LeftKind == IdentityMatchParticipant && candidate.RightKind == IdentityMatchParticipant &&
		candidate.LeftPerson != nil && candidate.RightPerson != nil &&
		candidate.LeftPerson.PersonID != candidate.RightPerson.PersonID {
		candidate.Actionable = false
		candidate.Blocker = "person_merge_required"
	}
}

// populateIdentityMatchReviewTokensContext batches the database reads used by
// review fingerprints. Listing a full review page should not issue a dozen
// network round trips per candidate, especially for remote PostgreSQL stores.
func populateIdentityMatchReviewTokensContext(
	ctx context.Context, q identityMatchReviewQuerier, candidates []IdentityMatchCandidate,
) error {
	if len(candidates) == 0 {
		return nil
	}
	data, err := loadIdentityMatchFingerprintDataContext(ctx, q, candidates)
	if err != nil {
		return err
	}
	for i := range candidates {
		candidate := &candidates[i]
		candidate.SourceSupport = make([]IdentityMatchSourceSupport, 0, len(data.candidateSources[candidate.ID]))
		for _, row := range data.candidateSources[candidate.ID] {
			if len(row) < 3 {
				return errors.New("batched identity match candidate source row was incomplete")
			}
			sourceID, idOK := row[1].(int64)
			isConservative, conservativeOK := identityMatchFingerprintBool(row[2])
			if !idOK || !conservativeOK {
				return errors.New("batched identity match candidate source row had invalid values")
			}
			candidate.SourceSupport = append(candidate.SourceSupport, IdentityMatchSourceSupport{
				SourceID: sourceID, IsConservative: isConservative,
			})
		}
		evidenceIndexes := make(map[int64]int, len(candidate.Evidence))
		for evidenceIndex := range candidate.Evidence {
			candidate.Evidence[evidenceIndex].SourceSupport = []IdentityMatchSourceSupport{}
			evidenceIndexes[candidate.Evidence[evidenceIndex].ID] = evidenceIndex
		}
		for _, row := range data.evidenceSources[candidate.ID] {
			if len(row) < 3 {
				return errors.New("batched identity match evidence source row was incomplete")
			}
			evidenceID, idOK := row[0].(int64)
			sourceID, sourceOK := row[1].(int64)
			isConservative, conservativeOK := identityMatchFingerprintBool(row[2])
			evidenceIndex, exists := evidenceIndexes[evidenceID]
			if !idOK || !sourceOK || !conservativeOK || !exists {
				return errors.New("batched identity match evidence source row had invalid values")
			}
			candidate.Evidence[evidenceIndex].SourceSupport = append(
				candidate.Evidence[evidenceIndex].SourceSupport,
				IdentityMatchSourceSupport{SourceID: sourceID, IsConservative: isConservative},
			)
		}
		if candidate.LeftKind == IdentityMatchParticipant && candidate.RightKind == IdentityMatchParticipant {
			leftPerson, hasLeftPerson, err := identityMatchFingerprintBinding(data.bindings[candidate.LeftID])
			if err != nil {
				return err
			}
			if hasLeftPerson {
				candidate.LeftPerson = leftPerson
			} else {
				candidate.LeftPerson = nil
			}
			rightPerson, hasRightPerson, err := identityMatchFingerprintBinding(data.bindings[candidate.RightID])
			if err != nil {
				return err
			}
			if hasRightPerson {
				candidate.RightPerson = rightPerson
			} else {
				candidate.RightPerson = nil
			}
			applyIdentityMatchReviewBindingBlocker(candidate)
		}
	}

	for i := range candidates {
		token, err := data.fingerprint(&candidates[i], true)
		if err != nil {
			return err
		}
		candidates[i].ReviewToken = token
	}
	return nil
}

type identityMatchFingerprintData struct {
	candidateSources, evidence, evidenceSources map[int64][][]any
	participants, identifiers, observations     map[int64][][]any
	componentMembers, bindings, publications    map[int64][][]any
	persons, personPublications, resources      map[int64][][]any
}

func loadIdentityMatchFingerprintDataContext(
	ctx context.Context, q identityMatchReviewQuerier, candidates []IdentityMatchCandidate,
) (*identityMatchFingerprintData, error) {
	data := &identityMatchFingerprintData{}
	var err error
	candidateIDs := make([]int64, 0, len(candidates))
	participantIDs := make(map[int64]struct{})
	personIDs := make(map[int64]struct{})
	resourceIDs := make(map[int64]struct{})
	for i := range candidates {
		candidate := &candidates[i]
		candidateIDs = append(candidateIDs, candidate.ID)
		for _, endpoint := range []struct {
			kind IdentityMatchEndpointKind
			id   int64
		}{{candidate.LeftKind, candidate.LeftID}, {candidate.RightKind, candidate.RightID}} {
			switch endpoint.kind {
			case IdentityMatchParticipant:
				participantIDs[endpoint.id] = struct{}{}
			case IdentityMatchPerson:
				personIDs[endpoint.id] = struct{}{}
			case IdentityMatchCardDAVResource:
				resourceIDs[endpoint.id] = struct{}{}
			case IdentityMatchObservation, IdentityMatchContactPoint:
				// These endpoint kinds have no materialized identity rows.
			}
		}
	}

	data.candidateSources, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT candidate_id, candidate_id, source_id, is_conservative FROM identity_match_candidate_sources WHERE candidate_id IN (%s) ORDER BY candidate_id, source_id`,
		candidateIDs)
	if err != nil {
		return nil, err
	}
	data.evidence, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT candidate_id, * FROM identity_match_evidence WHERE candidate_id IN (%s) ORDER BY candidate_id, id`,
		candidateIDs)
	if err != nil {
		return nil, err
	}
	data.evidenceSources, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT e.candidate_id, es.evidence_id, es.source_id, es.is_conservative FROM identity_match_evidence_sources es
		JOIN identity_match_evidence e ON e.id = es.evidence_id
		WHERE e.candidate_id IN (%s) ORDER BY e.candidate_id, es.evidence_id, es.source_id`,
		candidateIDs)
	if err != nil {
		return nil, err
	}

	data.participants, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT id, * FROM participants WHERE id IN (%s) ORDER BY id`, identityMatchInt64MapKeys(participantIDs))
	if err != nil {
		return nil, err
	}
	data.identifiers, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT participant_id, * FROM participant_identifiers WHERE participant_id IN (%s) ORDER BY participant_id, id`,
		identityMatchInt64MapKeys(participantIDs))
	if err != nil {
		return nil, err
	}
	data.observations, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT participant_id, * FROM participant_contact_observations WHERE participant_id IN (%s) ORDER BY participant_id, id`,
		identityMatchInt64MapKeys(participantIDs))
	if err != nil {
		return nil, err
	}
	data.componentMembers, data.bindings, data.publications, err = identityMatchFingerprintParticipantComponents(ctx, q, identityMatchInt64MapKeys(participantIDs))
	if err != nil {
		return nil, err
	}

	data.persons, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT id, * FROM persons WHERE id IN (%s) ORDER BY id`, identityMatchInt64MapKeys(personIDs))
	if err != nil {
		return nil, err
	}
	data.personPublications, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT person_id, person_id, desired, address_book_id, href, remote_etag, mapping_revision, mutation_revision, approved_body_sha256
		FROM carddav_publications WHERE person_id IN (%s) ORDER BY person_id`,
		identityMatchInt64MapKeys(personIDs))
	if err != nil {
		return nil, err
	}
	data.resources, err = identityMatchFingerprintRowsByKey(ctx, q,
		`SELECT id, id, address_book_id, href, remote_uid, remote_etag, remote_semantic_hash, mapping_status, mapping_revision, person_id, person_revision_at_bind
		FROM carddav_resources WHERE id IN (%s) ORDER BY id`, identityMatchInt64MapKeys(resourceIDs))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (data *identityMatchFingerprintData) fingerprint(candidate *IdentityMatchCandidate, includeDecision bool) (string, error) {
	h := sha256.New()
	base := []any{candidate.ID, candidate.LeftKind, candidate.LeftID, candidate.RightKind, candidate.RightID,
		candidate.Basis, candidate.ServiceSlug, candidate.ScopeKind, candidate.ScopeValue, candidate.NormalizedValue,
		candidate.Confidence, candidate.Source, candidate.SourceRef}
	if includeDecision {
		base = append(base, candidate.State, candidate.DecidedBy, candidate.DecidedAt, candidate.Notes,
			candidate.applicationPending,
			candidate.conflictState.observationOrigin.String, candidate.conflictState.observationOrigin.Valid,
			candidate.conflictState.preConflictState.String, candidate.conflictState.preConflictState.Valid)
	}
	if err := hashIdentityMatchValue(h, base); err != nil {
		return "", err
	}
	for _, group := range []struct {
		name string
		rows [][]any
	}{{"candidate_sources", data.candidateSources[candidate.ID]}, {"evidence", data.evidence[candidate.ID]}, {"evidence_sources", data.evidenceSources[candidate.ID]}} {
		if err := hashIdentityMatchRows(h, group.name, group.rows); err != nil {
			return "", err
		}
	}
	for _, endpoint := range []struct {
		kind IdentityMatchEndpointKind
		id   int64
	}{{candidate.LeftKind, candidate.LeftID}, {candidate.RightKind, candidate.RightID}} {
		var groups []struct {
			name string
			rows [][]any
		}
		switch endpoint.kind {
		case IdentityMatchParticipant:
			groups = []struct {
				name string
				rows [][]any
			}{{"participant", data.participants[endpoint.id]}, {"identifiers", data.identifiers[endpoint.id]},
				{"observations", data.observations[endpoint.id]}, {"component_members", data.componentMembers[endpoint.id]},
				{"bindings", data.bindings[endpoint.id]},
				{"bound_publications", data.publications[endpoint.id]}}
		case IdentityMatchPerson:
			groups = []struct {
				name string
				rows [][]any
			}{{string(AttributeObjectPerson), data.persons[endpoint.id]}, {"publication", data.personPublications[endpoint.id]}}
		case IdentityMatchCardDAVResource:
			groups = []struct {
				name string
				rows [][]any
			}{{"carddav_resource", data.resources[endpoint.id]}}
		case IdentityMatchObservation, IdentityMatchContactPoint:
			// These endpoint kinds have no materialized identity rows.
		}
		for _, group := range groups {
			if err := hashIdentityMatchRows(h, group.name, group.rows); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func identityMatchFingerprintRowsByKey(
	ctx context.Context, q identityMatchReviewQuerier, queryTemplate string, ids []int64,
) (map[int64][][]any, error) {
	result := make(map[int64][][]any, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	query := fmt.Sprintf(queryTemplate, placeholders)
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return identityMatchFingerprintRows(ctx, q, query, args)
}

func identityMatchFingerprintRows(
	ctx context.Context, q identityMatchReviewQuerier, query string, args []any,
) (map[int64][][]any, error) {
	result := make(map[int64][][]any)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load batched identity match fingerprint rows: %w", err)
	}
	for rows.Next() {
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			_ = rows.Close()
			return nil, err
		}
		key, ok := values[0].(int64)
		if !ok {
			_ = rows.Close()
			return nil, fmt.Errorf("batched identity match fingerprint key has type %T", values[0])
		}
		result[key] = append(result[key], values[1:])
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return result, nil
}

func hashIdentityMatchRows(h hash.Hash, name string, rows [][]any) error {
	if err := hashIdentityMatchValue(h, name); err != nil {
		return err
	}
	for _, row := range rows {
		if err := hashIdentityMatchValue(h, row); err != nil {
			return err
		}
	}
	return nil
}

func identityMatchInt64MapKeys(values map[int64]struct{}) []int64 {
	result := make([]int64, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

func identityMatchFingerprintBinding(rows [][]any) (*IdentityMatchPersonBinding, bool, error) {
	if len(rows) == 0 {
		return nil, false, nil
	}
	if len(rows) != 1 || len(rows[0]) < 3 {
		return nil, false, errors.New("identity cluster has multiple or incomplete person bindings")
	}
	personID, personOK := rows[0][0].(int64)
	revision, revisionOK := rows[0][1].(int64)
	publication, publicationOK := identityMatchFingerprintBool(rows[0][2])
	if !personOK || !revisionOK || !publicationOK {
		return nil, false, errors.New("identity cluster person binding had invalid values")
	}
	return &IdentityMatchPersonBinding{
		PersonID: personID, Revision: revision, ActiveCardDAVPublication: publication,
	}, true, nil
}

func identityMatchFingerprintBool(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case int64:
		if value == 0 || value == 1 {
			return value == 1, true
		}
	}
	return false, false
}

func identityMatchFingerprintParticipantComponents(
	ctx context.Context, q identityMatchReviewQuerier, participantIDs []int64,
) (map[int64][][]any, map[int64][][]any, map[int64][][]any, error) {
	componentMembers := make(map[int64][][]any, len(participantIDs))
	bindings := make(map[int64][][]any, len(participantIDs))
	publications := make(map[int64][][]any, len(participantIDs))
	if len(participantIDs) == 0 {
		return componentMembers, bindings, publications, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(participantIDs)), ",")
	args := make([]any, len(participantIDs))
	for i, id := range participantIDs {
		args[i] = id
	}
	roots := fmt.Sprintf(`WITH RECURSIVE roots(root_id) AS (SELECT id FROM participants WHERE id IN (%s)),
		component(root_id, participant_id) AS (
			SELECT root_id, root_id FROM roots
			UNION
			SELECT component.root_id, CASE WHEN l.participant_a = component.participant_id
				THEN l.participant_b ELSE l.participant_a END
			FROM component JOIN participant_links l
				ON l.participant_a = component.participant_id OR l.participant_b = component.participant_id
		), bound_people(root_id, person_id) AS (
			SELECT component.root_id, pp.person_id
			FROM component JOIN person_participants pp ON pp.participant_id = component.participant_id
			GROUP BY component.root_id, pp.person_id
		)
		`, placeholders)
	// Membership determines the scope of a link, including participants without person bindings.
	memberQuery := roots + `SELECT root_id, participant_id FROM component ORDER BY root_id, participant_id`
	memberRows, err := identityMatchFingerprintRows(ctx, q, memberQuery, args)
	if err != nil {
		return nil, nil, nil, err
	}
	bindingQuery := roots + `SELECT bound_people.root_id, p.id, p.revision, EXISTS (
		SELECT 1 FROM carddav_publications cp WHERE cp.person_id = p.id
			AND (cp.desired = TRUE OR cp.pending_operation IS NOT NULL)
	) FROM bound_people JOIN persons p ON p.id = bound_people.person_id
	ORDER BY bound_people.root_id, p.id`
	bindingRows, err := identityMatchFingerprintRows(ctx, q, bindingQuery, args)
	if err != nil {
		return nil, nil, nil, err
	}
	// Creation/update timestamps record persistence activity, not changed review inputs.
	publicationQuery := roots + `SELECT bound_people.root_id,
		cp.person_id, cp.desired, cp.address_book_id, cp.href, cp.pending_operation,
		cp.outgoing_body, cp.outgoing_semantic_hash, cp.local_hash, cp.remote_etag,
		cp.connection_generation, cp.book_sync_revision, cp.mapping_revision,
		cp.previous_mapping_revision, cp.create_recovery_used, cp.mutation_revision,
		cp.outgoing_envelope_metadata, cp.approved_body_sha256, cp.approved_inference_revision,
		cp.approved_mutation_revision, cp.pending_started_at
	FROM bound_people JOIN carddav_publications cp ON cp.person_id = bound_people.person_id
	ORDER BY bound_people.root_id, cp.person_id`
	publicationRows, err := identityMatchFingerprintRows(ctx, q, publicationQuery, args)
	if err != nil {
		return nil, nil, nil, err
	}
	return memberRows, bindingRows, publicationRows, nil
}

const identityMatchClusterBindingsSQL = `WITH RECURSIVE component(id) AS (
	SELECT CAST(? AS BIGINT) UNION
	SELECT CASE WHEN l.participant_a = component.id THEN l.participant_b ELSE l.participant_a END
	FROM participant_links l JOIN component ON l.participant_a = component.id OR l.participant_b = component.id
)
SELECT p.id, p.revision, EXISTS (
	SELECT 1 FROM carddav_publications cp WHERE cp.person_id = p.id
		AND (cp.desired = TRUE OR cp.pending_operation IS NOT NULL)
) FROM persons p WHERE EXISTS (
	SELECT 1 FROM person_participants pp
	WHERE pp.person_id = p.id AND pp.participant_id IN (SELECT id FROM component)
) ORDER BY p.id`

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
	data, err := loadIdentityMatchFingerprintDataContext(ctx, q, []IdentityMatchCandidate{*c})
	if err != nil {
		return "", err
	}
	return data.fingerprint(c, includeDecision)
}

func identityMatchReviewReceiptMatchesTxContext(
	ctx context.Context, tx *loggedTx, candidate *IdentityMatchCandidate,
	decision IdentityMatchState, reviewToken string,
) (bool, error) {
	var recordedDecision IdentityMatchState
	var recordedToken, recordedFingerprint string
	err := tx.QueryRowContext(ctx, `SELECT decision, review_token, evidence_fingerprint
		FROM identity_match_review_receipts WHERE candidate_id = ?`, candidate.ID).Scan(
		&recordedDecision, &recordedToken, &recordedFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read identity match review receipt: %w", err)
	}
	if recordedDecision != decision || recordedToken != reviewToken {
		return false, nil
	}
	currentFingerprint, err := identityMatchFingerprintContext(ctx, tx, candidate, false)
	if err != nil {
		return false, err
	}
	return currentFingerprint == recordedFingerprint, nil
}

func recordIdentityMatchReviewReceiptTxContext(
	ctx context.Context, tx *loggedTx, candidateID int64,
	decision IdentityMatchState, reviewToken string,
) error {
	candidate, err := getIdentityMatchCandidateTx(ctx, tx, candidateID)
	if err != nil {
		return err
	}
	fingerprint, err := identityMatchFingerprintContext(ctx, tx, candidate, false)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identity_match_review_receipts
		(candidate_id, decision, review_token, evidence_fingerprint) VALUES (?, ?, ?, ?)
		ON CONFLICT (candidate_id) DO UPDATE SET decision = excluded.decision,
			review_token = excluded.review_token,
			evidence_fingerprint = excluded.evidence_fingerprint`,
		candidateID, decision, reviewToken, fingerprint); err != nil {
		return fmt.Errorf("persist identity match review receipt: %w", err)
	}
	return nil
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
		if errors.Is(err, ErrIdentityMatchNotAccepted) {
			return nil, 0, ErrIdentityMatchReviewStale
		}
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
