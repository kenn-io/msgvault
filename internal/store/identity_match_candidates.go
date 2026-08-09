package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type IdentityMatchEndpointKind string

const (
	IdentityMatchParticipant  IdentityMatchEndpointKind = "participant"
	IdentityMatchPerson       IdentityMatchEndpointKind = "person"
	IdentityMatchObservation  IdentityMatchEndpointKind = "observation"
	IdentityMatchContactPoint IdentityMatchEndpointKind = "contact_point"
)

func (k IdentityMatchEndpointKind) valid() bool {
	switch k {
	case IdentityMatchParticipant, IdentityMatchPerson,
		IdentityMatchObservation, IdentityMatchContactPoint:
		return true
	default:
		return false
	}
}

type IdentityMatchBasis string

const (
	IdentityMatchStableProviderID       IdentityMatchBasis = "stable_provider_id"
	IdentityMatchServiceScopeUsername   IdentityMatchBasis = "service_scope_username"
	IdentityMatchEmail                  IdentityMatchBasis = "email"
	IdentityMatchPhone                  IdentityMatchBasis = "phone"
	IdentityMatchDisplayName            IdentityMatchBasis = "display_name"
	IdentityMatchConversationMembership IdentityMatchBasis = "conversation_membership"
)

func (b IdentityMatchBasis) valid() bool {
	switch b {
	case IdentityMatchStableProviderID, IdentityMatchServiceScopeUsername,
		IdentityMatchEmail, IdentityMatchPhone, IdentityMatchDisplayName,
		IdentityMatchConversationMembership:
		return true
	default:
		return false
	}
}

type IdentityMatchState string

const (
	IdentityMatchStateCandidate IdentityMatchState = "candidate"
	IdentityMatchStateAccepted  IdentityMatchState = "accepted"
	IdentityMatchStateRejected  IdentityMatchState = "rejected"
	IdentityMatchStateConflict  IdentityMatchState = "conflict"

	observationConflictOriginGenerated = "generated"
	observationConflictOriginPromoted  = "promoted"
)

func (s IdentityMatchState) valid() bool {
	switch s {
	case IdentityMatchStateCandidate, IdentityMatchStateAccepted,
		IdentityMatchStateRejected, IdentityMatchStateConflict:
		return true
	default:
		return false
	}
}

type IdentityMatchCandidate struct {
	ID              int64                     `json:"id"`
	LeftKind        IdentityMatchEndpointKind `json:"left_kind"`
	LeftID          int64                     `json:"left_id"`
	RightKind       IdentityMatchEndpointKind `json:"right_kind"`
	RightID         int64                     `json:"right_id"`
	Basis           IdentityMatchBasis        `json:"basis"`
	ServiceSlug     *string                   `json:"service_slug,omitempty"`
	ScopeKind       *string                   `json:"scope_kind,omitempty"`
	ScopeValue      *string                   `json:"scope_value,omitempty"`
	NormalizedValue *string                   `json:"normalized_value,omitempty"`
	State           IdentityMatchState        `json:"state"`
	Confidence      *float64                  `json:"confidence,omitempty"`
	Source          Provenance                `json:"source"`
	SourceRef       *string                   `json:"source_ref,omitempty"`
	DecidedBy       *string                   `json:"decided_by,omitempty"`
	DecidedAt       *time.Time                `json:"decided_at,omitempty"`
	Notes           *string                   `json:"notes,omitempty"`
	Evidence        []IdentityMatchEvidence   `json:"evidence"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
}

type IdentityMatchEvidence struct {
	ID           int64      `json:"id"`
	CandidateID  int64      `json:"candidate_id"`
	EvidenceKind string     `json:"evidence_kind"`
	EvidenceRef  *string    `json:"evidence_ref,omitempty"`
	Detail       *string    `json:"detail,omitempty"`
	Source       Provenance `json:"source"`
	CreatedAt    time.Time  `json:"created_at"`
}

type IdentityMatchCandidateInput struct {
	LeftKind        IdentityMatchEndpointKind
	LeftID          int64
	RightKind       IdentityMatchEndpointKind
	RightID         int64
	Basis           IdentityMatchBasis
	ServiceSlug     *string
	ScopeKind       *string
	ScopeValue      *string
	NormalizedValue *string
	State           IdentityMatchState
	Confidence      *float64
	Source          Provenance
	SourceRef       *string
	Notes           *string
}

type IdentityMatchEvidenceInput struct {
	EvidenceKind string
	EvidenceRef  *string
	Detail       *string
	Source       Provenance
}

var (
	ErrInvalidIdentityMatchEndpoint  = errors.New("invalid identity match endpoint kind")
	ErrInvalidIdentityMatchBasis     = errors.New("invalid identity match basis")
	ErrInvalidIdentityMatchState     = errors.New("invalid identity match state")
	ErrIdentityMatchSelfLink         = errors.New("identity match endpoints must differ")
	ErrIdentityMatchNotFound         = errors.New("identity match candidate not found")
	ErrIdentityMatchEndpointNotFound = errors.New("identity match endpoint not found")
	ErrIdentityMatchNotAcceptable    = errors.New("a username-only match requires stable provider corroboration or explicit confirmation")
)

func (s *Store) UpsertIdentityMatchCandidateContext(
	ctx context.Context, input IdentityMatchCandidateInput,
) (*IdentityMatchCandidate, bool, error) {
	if !input.LeftKind.valid() || !input.RightKind.valid() {
		return nil, false, ErrInvalidIdentityMatchEndpoint
	}
	if !input.Basis.valid() {
		return nil, false, ErrInvalidIdentityMatchBasis
	}
	if !input.State.valid() {
		return nil, false, ErrInvalidIdentityMatchState
	}
	if input.State != IdentityMatchStateCandidate && input.State != IdentityMatchStateConflict {
		return nil, false, ErrInvalidIdentityMatchState
	}
	if !input.Source.Valid() {
		return nil, false, ErrInvalidProvenance
	}
	if input.Confidence != nil {
		if err := (ValueEnvelope{Source: input.Source, Confidence: input.Confidence}).Validate(); err != nil {
			return nil, false, err
		}
	}
	leftKind, leftID, rightKind, rightID, err := canonicalMatchEndpoints(
		input.LeftKind, input.LeftID, input.RightKind, input.RightID,
	)
	if err != nil {
		return nil, false, err
	}
	input.ScopeKind = normalizeScopeInput(input.ScopeKind)
	input.ScopeValue = normalizeScopeInput(input.ScopeValue)
	service, hasService, err := s.resolveOptionalCommunicationServiceContext(ctx, input.ServiceSlug)
	if err != nil {
		return nil, false, err
	}
	var serviceID any
	if hasService {
		serviceID = service.ID
	}
	var candidate *IdentityMatchCandidate
	created := false
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := validateIdentityMatchEndpointTx(ctx, tx, leftKind, leftID); err != nil {
			return err
		}
		if err := validateIdentityMatchEndpointTx(ctx, tx, rightKind, rightID); err != nil {
			return err
		}
		candidate, created, err = s.upsertIdentityMatchCandidateTx(
			ctx, tx, input, leftKind, leftID, rightKind, rightID, serviceID, false,
		)
		return err
	})
	return candidate, created, err
}

func validateIdentityMatchEndpointTx(
	ctx context.Context, tx *loggedTx, kind IdentityMatchEndpointKind, id int64,
) error {
	var table string
	switch kind {
	case IdentityMatchParticipant:
		table = "participants"
	case IdentityMatchPerson:
		table = "persons"
	case IdentityMatchObservation:
		table = "participant_contact_observations"
	case IdentityMatchContactPoint:
		table = "person_contact_points"
	default:
		return ErrInvalidIdentityMatchEndpoint
	}
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id,
	).Scan(&exists); err != nil {
		return fmt.Errorf("validate %s identity match endpoint: %w", kind, err)
	}
	if exists == 0 {
		return fmt.Errorf("%w: %s %d", ErrIdentityMatchEndpointNotFound, kind, id)
	}
	return nil
}

func (s *Store) upsertIdentityMatchCandidateTx(
	ctx context.Context,
	tx *loggedTx,
	input IdentityMatchCandidateInput,
	leftKind IdentityMatchEndpointKind,
	leftID int64,
	rightKind IdentityMatchEndpointKind,
	rightID int64,
	serviceID any,
	observationConflict bool,
) (*IdentityMatchCandidate, bool, error) {
	if err := s.lockProfileIdentityKeyTxContext(
		ctx, tx, "identity-match-candidate",
		leftKind, leftID, rightKind, rightID, input.Basis, serviceID,
		stringValue(input.ScopeKind), stringValue(input.ScopeValue),
		stringValue(input.NormalizedValue),
	); err != nil {
		return nil, false, err
	}
	candidate, err := findIdentityMatchCandidateTx(
		ctx, tx, leftKind, leftID, rightKind, rightID, input.Basis,
		serviceID, input.ScopeKind, input.ScopeValue, input.NormalizedValue,
		s.dialect.SelectForUpdate(),
	)
	if err == nil {
		if candidate.State == IdentityMatchStateCandidate &&
			input.State == IdentityMatchStateConflict {
			query := `UPDATE identity_match_candidates SET
				state = ?, confidence = COALESCE(?, confidence), source = ?, source_ref = ?,
				updated_at = ` + s.dialect.Now() + ` WHERE id = ?`
			args := []any{
				input.State, floatValue(input.Confidence), input.Source,
				stringValue(input.SourceRef), candidate.ID,
			}
			if observationConflict {
				query = `UPDATE identity_match_candidates SET
					state = ?, observation_conflict_origin = ?,
					updated_at = ` + s.dialect.Now() + ` WHERE id = ?`
				args = []any{
					input.State, observationConflictOriginPromoted, candidate.ID,
				}
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return nil, false, fmt.Errorf("promote identity match candidate to conflict: %w", err)
			}
			candidate, err = getIdentityMatchCandidateTx(ctx, tx, candidate.ID)
			return candidate, false, err
		}
		return candidate, false, nil
	}
	if !errors.Is(err, ErrIdentityMatchNotFound) {
		return nil, false, err
	}
	var observationOrigin any
	if observationConflict {
		observationOrigin = observationConflictOriginGenerated
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO identity_match_candidates (
		left_kind, left_id, right_kind, right_id, basis, service_id,
		scope_kind, scope_value, normalized_value, state, confidence,
		source, source_ref, observation_conflict_origin, notes, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`) RETURNING id`,
		leftKind, leftID, rightKind, rightID, input.Basis, serviceID,
		stringValue(input.ScopeKind), stringValue(input.ScopeValue),
		stringValue(input.NormalizedValue), input.State, floatValue(input.Confidence),
		input.Source, stringValue(input.SourceRef), observationOrigin, stringValue(input.Notes),
	).Scan(&id); err != nil {
		return nil, false, fmt.Errorf("insert identity match candidate: %w", err)
	}
	candidate, err = getIdentityMatchCandidateTx(ctx, tx, id)
	return candidate, err == nil, err
}

func (s *Store) AddIdentityMatchEvidenceContext(
	ctx context.Context, candidateID int64, input IdentityMatchEvidenceInput,
) (*IdentityMatchEvidence, error) {
	kind := strings.TrimSpace(input.EvidenceKind)
	if kind == "" || !input.Source.Valid() {
		if !input.Source.Valid() {
			return nil, ErrInvalidProvenance
		}
		return nil, errors.New("identity match evidence kind is required")
	}
	var evidence *IdentityMatchEvidence
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM identity_match_candidates WHERE id = ?`, candidateID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check identity match candidate: %w", err)
		}
		if exists == 0 {
			return ErrIdentityMatchNotFound
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO identity_match_evidence (
			candidate_id, evidence_kind, evidence_ref, detail, source
		) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			candidateID, kind, stringValue(input.EvidenceRef),
			stringValue(input.Detail), input.Source,
		).Scan(&id); err != nil {
			return fmt.Errorf("add identity match evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates
			SET updated_at = `+s.dialect.Now()+` WHERE id = ?`, candidateID,
		); err != nil {
			return fmt.Errorf("touch identity match candidate: %w", err)
		}
		var err error
		evidence, err = getIdentityMatchEvidenceTx(ctx, tx, id)
		return err
	})
	return evidence, err
}

func (s *Store) ListIdentityMatchCandidatesContext(
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
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list identity match candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]IdentityMatchCandidate, 0)
	for rows.Next() {
		candidate, err := scanIdentityMatchCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan identity match candidate: %w", err)
		}
		candidates = append(candidates, *candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list identity match candidates: %w", err)
	}
	if err := s.loadCandidateEvidencePageContext(ctx, candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (s *Store) DecideIdentityMatchCandidateContext(
	ctx context.Context,
	candidateID int64,
	state IdentityMatchState,
	decidedBy string,
	notes *string,
) (*IdentityMatchCandidate, error) {
	if !state.valid() {
		return nil, ErrInvalidIdentityMatchState
	}
	var candidate *IdentityMatchCandidate
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := getIdentityMatchCandidateTx(ctx, tx, candidateID)
		if err != nil {
			return err
		}
		// Only a stable-provider-id candidate that records which stable ID
		// matched may be accepted without explicit user confirmation; the basis
		// label alone is caller-supplied and proves nothing.
		if state == IdentityMatchStateAccepted && decidedBy != "user" &&
			(current.Basis != IdentityMatchStableProviderID ||
				current.NormalizedValue == nil) {
			return ErrIdentityMatchNotAcceptable
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates SET
			state = ?, decided_by = ?, decided_at = `+s.dialect.Now()+`,
			notes = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`,
			state, decidedBy, stringValue(notes), candidateID,
		); err != nil {
			return fmt.Errorf("decide identity match candidate: %w", err)
		}
		candidate, err = getIdentityMatchCandidateTx(ctx, tx, candidateID)
		return err
	})
	return candidate, err
}

type identityMatchCandidateMergeRow struct {
	ID                        int64
	LeftKind                  IdentityMatchEndpointKind
	LeftID                    int64
	RightKind                 IdentityMatchEndpointKind
	RightID                   int64
	Basis                     IdentityMatchBasis
	ServiceID                 sql.NullInt64
	ScopeKind                 sql.NullString
	ScopeValue                sql.NullString
	NormalizedValue           sql.NullString
	State                     IdentityMatchState
	Confidence                sql.NullFloat64
	Source                    Provenance
	SourceRef                 sql.NullString
	ObservationConflictOrigin sql.NullString
	DecidedBy                 sql.NullString
	DecidedAt                 sql.NullTime
	Notes                     sql.NullString
}

const identityMatchCandidateMergeSelect = `SELECT
	id, left_kind, left_id, right_kind, right_id, basis, service_id,
	scope_kind, scope_value, normalized_value, state, confidence, source, source_ref,
	observation_conflict_origin, decided_by, decided_at, notes
	FROM identity_match_candidates`

func scanIdentityMatchCandidateMergeRow(row scanner) (identityMatchCandidateMergeRow, error) {
	var candidate identityMatchCandidateMergeRow
	err := row.Scan(
		&candidate.ID, &candidate.LeftKind, &candidate.LeftID,
		&candidate.RightKind, &candidate.RightID, &candidate.Basis,
		&candidate.ServiceID, &candidate.ScopeKind, &candidate.ScopeValue,
		&candidate.NormalizedValue, &candidate.State,
		&candidate.Confidence, &candidate.Source,
		&candidate.SourceRef, &candidate.ObservationConflictOrigin,
		&candidate.DecidedBy,
		&candidate.DecidedAt, &candidate.Notes,
	)
	return candidate, err
}

func scanIdentityMatchCandidateMergeRows(
	rows *loggedRows,
) ([]identityMatchCandidateMergeRow, error) {
	defer func() { _ = rows.Close() }()
	candidates := make([]identityMatchCandidateMergeRow, 0)
	for rows.Next() {
		candidate, err := scanIdentityMatchCandidateMergeRow(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) rewriteIdentityMatchCandidatesForMergeTx(
	ctx context.Context, tx *loggedTx, oldID, newID int64,
) error {
	rows, err := tx.QueryContext(ctx, identityMatchCandidateMergeSelect+`
		WHERE (left_kind = ? AND left_id = ?)
		   OR (right_kind = ? AND right_id = ?)
		ORDER BY id`+s.dialect.SelectForUpdate(),
		IdentityMatchParticipant, oldID, IdentityMatchParticipant, oldID,
	)
	if err != nil {
		return fmt.Errorf("load identity match candidates for participant merge: %w", err)
	}
	candidates, err := scanIdentityMatchCandidateMergeRows(rows)
	if err != nil {
		return fmt.Errorf("scan identity match candidates for participant merge: %w", err)
	}

	for _, candidate := range candidates {
		if candidate.LeftKind == IdentityMatchParticipant && candidate.LeftID == oldID {
			candidate.LeftID = newID
		}
		if candidate.RightKind == IdentityMatchParticipant && candidate.RightID == oldID {
			candidate.RightID = newID
		}
		leftKind, leftID, rightKind, rightID, canonicalErr := canonicalMatchEndpoints(
			candidate.LeftKind, candidate.LeftID, candidate.RightKind, candidate.RightID,
		)
		if errors.Is(canonicalErr, ErrIdentityMatchSelfLink) {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM identity_match_candidates WHERE id = ?`, candidate.ID,
			); err != nil {
				return fmt.Errorf("remove merged identity match self-link: %w", err)
			}
			continue
		}
		if canonicalErr != nil {
			return canonicalErr
		}
		candidate.LeftKind, candidate.LeftID = leftKind, leftID
		candidate.RightKind, candidate.RightID = rightKind, rightID

		collisions, err := s.loadIdentityMatchCandidateMergeCollisionsTx(ctx, tx, candidate)
		if err != nil {
			return err
		}
		group := append([]identityMatchCandidateMergeRow{candidate}, collisions...)
		if len(group) == 1 {
			if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates SET
				left_kind = ?, left_id = ?, right_kind = ?, right_id = ?,
				updated_at = `+s.dialect.Now()+` WHERE id = ?`,
				leftKind, leftID, rightKind, rightID, candidate.ID,
			); err != nil {
				return fmt.Errorf("rewrite identity match candidate endpoints: %w", err)
			}
			continue
		}

		if err := s.collapseIdentityMatchCandidateMergeGroupTx(ctx, tx, group); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadIdentityMatchCandidateMergeCollisionsTx(
	ctx context.Context, tx *loggedTx, candidate identityMatchCandidateMergeRow,
) ([]identityMatchCandidateMergeRow, error) {
	rows, err := tx.QueryContext(ctx, identityMatchCandidateMergeSelect+`
		WHERE id <> ? AND left_kind = ? AND left_id = ?
		  AND right_kind = ? AND right_id = ? AND basis = ?
		  AND (service_id = ? OR (service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (scope_kind = ? OR (scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (scope_value = ? OR (scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (normalized_value = ? OR
		       (normalized_value IS NULL AND CAST(? AS TEXT) IS NULL))
		ORDER BY id`+s.dialect.SelectForUpdate(),
		candidate.ID, candidate.LeftKind, candidate.LeftID,
		candidate.RightKind, candidate.RightID, candidate.Basis,
		candidate.ServiceID, candidate.ServiceID,
		candidate.ScopeKind, candidate.ScopeKind,
		candidate.ScopeValue, candidate.ScopeValue,
		candidate.NormalizedValue, candidate.NormalizedValue,
	)
	if err != nil {
		return nil, fmt.Errorf("load identity match candidate merge collisions: %w", err)
	}
	collisions, err := scanIdentityMatchCandidateMergeRows(rows)
	if err != nil {
		return nil, fmt.Errorf("scan identity match candidate merge collisions: %w", err)
	}
	return collisions, nil
}

func (s *Store) collapseIdentityMatchCandidateMergeGroupTx(
	ctx context.Context, tx *loggedTx, group []identityMatchCandidateMergeRow,
) error {
	sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
	winner := group[0]
	state, decidedBy, decidedAt, notes := reconcileIdentityMatchCandidateMergeState(group)
	confidence, source, sourceRef := identityMatchCandidateMergeConfidenceProvenance(group)
	observationOrigin := reconcileIdentityMatchCandidateMergeObservationOrigin(group, state)

	for _, loser := range group[1:] {
		if _, err := tx.ExecContext(ctx, `UPDATE identity_match_evidence
			SET candidate_id = ? WHERE candidate_id = ?`, winner.ID, loser.ID,
		); err != nil {
			return fmt.Errorf("move identity match candidate evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM identity_match_candidates WHERE id = ?`, loser.ID,
		); err != nil {
			return fmt.Errorf("remove duplicate identity match candidate: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates SET
		left_kind = ?, left_id = ?, right_kind = ?, right_id = ?, state = ?,
		confidence = ?, source = ?, source_ref = ?,
		observation_conflict_origin = ?, decided_by = ?, decided_at = ?, notes = ?,
		updated_at = `+s.dialect.Now()+` WHERE id = ?`,
		winner.LeftKind, winner.LeftID, winner.RightKind, winner.RightID, state,
		confidence, source, sourceRef, observationOrigin,
		decidedBy, decidedAt, notes, winner.ID,
	); err != nil {
		return fmt.Errorf("reconcile duplicate identity match candidate: %w", err)
	}
	return nil
}

func reconcileIdentityMatchCandidateMergeObservationOrigin(
	group []identityMatchCandidateMergeRow,
	state IdentityMatchState,
) sql.NullString {
	if state != IdentityMatchStateConflict {
		return sql.NullString{}
	}
	hasObservationConflict := false
	allGenerated := true
	for _, candidate := range group {
		if candidate.State == IdentityMatchStateConflict &&
			candidate.ObservationConflictOrigin.Valid {
			hasObservationConflict = true
			if candidate.ObservationConflictOrigin.String != observationConflictOriginGenerated {
				allGenerated = false
			}
			continue
		}
		allGenerated = false
	}
	if !hasObservationConflict {
		return sql.NullString{}
	}
	if allGenerated {
		return sql.NullString{String: observationConflictOriginGenerated, Valid: true}
	}
	return sql.NullString{String: observationConflictOriginPromoted, Valid: true}
}

func reconcileIdentityMatchCandidateMergeState(
	group []identityMatchCandidateMergeRow,
) (IdentityMatchState, sql.NullString, sql.NullTime, sql.NullString) {
	hasAccepted, hasRejected := false, false
	state := IdentityMatchStateCandidate
	for _, candidate := range group {
		switch candidate.State {
		case IdentityMatchStateCandidate:
			// Candidate is the neutral state when terminal decisions are merged.
		case IdentityMatchStateConflict:
			state = IdentityMatchStateConflict
		case IdentityMatchStateAccepted:
			hasAccepted = true
		case IdentityMatchStateRejected:
			hasRejected = true
		}
	}
	generatedConflict := state != IdentityMatchStateConflict && hasAccepted && hasRejected
	if generatedConflict {
		return IdentityMatchStateConflict,
			sql.NullString{String: "system", Valid: true},
			sql.NullTime{Time: time.Now().UTC(), Valid: true},
			sql.NullString{
				String: "participant merge reconciled opposing identity decisions",
				Valid:  true,
			}
	}
	if state != IdentityMatchStateConflict {
		switch {
		case hasAccepted:
			state = IdentityMatchStateAccepted
		case hasRejected:
			state = IdentityMatchStateRejected
		}
	}
	for _, candidate := range group {
		if candidate.State == state && hasMergeDecisionMetadata(candidate) {
			return state, candidate.DecidedBy, candidate.DecidedAt, candidate.Notes
		}
	}
	// A conflict outranks terminal decisions when states collapse, but the
	// review that produced those decisions must survive the merge: without
	// this, a later conflict cleanup would demote the row to an undecided
	// candidate with no trace of who decided it.
	for _, candidate := range group {
		if (candidate.State == IdentityMatchStateAccepted ||
			candidate.State == IdentityMatchStateRejected) &&
			hasMergeDecisionMetadata(candidate) {
			return state, candidate.DecidedBy, candidate.DecidedAt, candidate.Notes
		}
	}
	for _, candidate := range group {
		if hasMergeDecisionMetadata(candidate) {
			return state, candidate.DecidedBy, candidate.DecidedAt, candidate.Notes
		}
	}
	return state, sql.NullString{}, sql.NullTime{}, sql.NullString{}
}

func hasMergeDecisionMetadata(candidate identityMatchCandidateMergeRow) bool {
	return candidate.DecidedBy.Valid || candidate.DecidedAt.Valid || candidate.Notes.Valid
}

func identityMatchCandidateMergeConfidenceProvenance(
	group []identityMatchCandidateMergeRow,
) (sql.NullFloat64, Provenance, sql.NullString) {
	var confidence sql.NullFloat64
	source, sourceRef := group[0].Source, group[0].SourceRef
	for _, candidate := range group {
		if candidate.Confidence.Valid &&
			(!confidence.Valid || candidate.Confidence.Float64 > confidence.Float64) {
			confidence = candidate.Confidence
			source, sourceRef = candidate.Source, candidate.SourceRef
		}
	}
	return confidence, source, sourceRef
}

func canonicalMatchEndpoints(
	leftKind IdentityMatchEndpointKind,
	leftID int64,
	rightKind IdentityMatchEndpointKind,
	rightID int64,
) (IdentityMatchEndpointKind, int64, IdentityMatchEndpointKind, int64, error) {
	if leftKind == rightKind && leftID == rightID {
		return "", 0, "", 0, ErrIdentityMatchSelfLink
	}
	if string(leftKind) > string(rightKind) ||
		(leftKind == rightKind && leftID > rightID) {
		return rightKind, rightID, leftKind, leftID, nil
	}
	return leftKind, leftID, rightKind, rightID, nil
}

const identityMatchCandidateSelect = `SELECT
	c.id, c.left_kind, c.left_id, c.right_kind, c.right_id, c.basis,
	cs.slug, c.scope_kind, c.scope_value, c.normalized_value, c.state,
	c.confidence, c.source, c.source_ref, c.decided_by, c.decided_at,
	c.notes, c.created_at, c.updated_at
	FROM identity_match_candidates c
	LEFT JOIN communication_services cs ON cs.id = c.service_id`

func findIdentityMatchCandidateTx(
	ctx context.Context,
	tx *loggedTx,
	leftKind IdentityMatchEndpointKind,
	leftID int64,
	rightKind IdentityMatchEndpointKind,
	rightID int64,
	basis IdentityMatchBasis,
	serviceID any,
	scopeKind, scopeValue, normalizedValue *string,
	lockClause string,
) (*IdentityMatchCandidate, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM identity_match_candidates
		WHERE left_kind = ? AND left_id = ?
		  AND right_kind = ? AND right_id = ? AND basis = ?
		  AND (service_id = ? OR (service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (scope_kind = ? OR (scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (scope_value = ? OR (scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (normalized_value = ? OR
		       (normalized_value IS NULL AND CAST(? AS TEXT) IS NULL))`+lockClause,
		leftKind, leftID, rightKind, rightID, basis,
		serviceID, serviceID,
		stringValue(scopeKind), stringValue(scopeKind),
		stringValue(scopeValue), stringValue(scopeValue),
		stringValue(normalizedValue), stringValue(normalizedValue),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrIdentityMatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find identity match candidate: %w", err)
	}
	return getIdentityMatchCandidateTx(ctx, tx, id)
}

func getIdentityMatchCandidateTx(
	ctx context.Context, tx *loggedTx, id int64,
) (*IdentityMatchCandidate, error) {
	candidate, err := scanIdentityMatchCandidate(tx.QueryRowContext(ctx,
		identityMatchCandidateSelect+` WHERE c.id = ?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrIdentityMatchNotFound
	}
	if err != nil {
		return nil, err
	}
	candidate.Evidence, err = loadCandidateEvidenceTx(ctx, tx, id)
	return candidate, err
}

func scanIdentityMatchCandidate(row scanner) (*IdentityMatchCandidate, error) {
	var candidate IdentityMatchCandidate
	var serviceSlug, scopeKind, scopeValue, normalizedValue sql.NullString
	var confidence sql.NullFloat64
	var sourceRef, decidedBy, notes sql.NullString
	var decidedAt sql.NullTime
	if err := row.Scan(
		&candidate.ID, &candidate.LeftKind, &candidate.LeftID,
		&candidate.RightKind, &candidate.RightID, &candidate.Basis,
		&serviceSlug, &scopeKind, &scopeValue, &normalizedValue,
		&candidate.State, &confidence, &candidate.Source, &sourceRef,
		&decidedBy, &decidedAt, &notes, &candidate.CreatedAt, &candidate.UpdatedAt,
	); err != nil {
		return nil, err
	}
	candidate.ServiceSlug = nullStringPtr(serviceSlug)
	candidate.ScopeKind = nullStringPtr(scopeKind)
	candidate.ScopeValue = nullStringPtr(scopeValue)
	candidate.NormalizedValue = nullStringPtr(normalizedValue)
	candidate.Confidence = nullFloatPtr(confidence)
	candidate.SourceRef = nullStringPtr(sourceRef)
	candidate.DecidedBy = nullStringPtr(decidedBy)
	candidate.DecidedAt = nullTimePtr(decidedAt)
	candidate.Notes = nullStringPtr(notes)
	candidate.Evidence = []IdentityMatchEvidence{}
	return &candidate, nil
}

func getIdentityMatchEvidenceTx(
	ctx context.Context, tx *loggedTx, id int64,
) (*IdentityMatchEvidence, error) {
	var evidence IdentityMatchEvidence
	var evidenceRef, detail sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT
		id, candidate_id, evidence_kind, evidence_ref, detail, source, created_at
		FROM identity_match_evidence WHERE id = ?`, id,
	).Scan(
		&evidence.ID, &evidence.CandidateID, &evidence.EvidenceKind,
		&evidenceRef, &detail, &evidence.Source, &evidence.CreatedAt,
	); err != nil {
		return nil, err
	}
	evidence.EvidenceRef = nullStringPtr(evidenceRef)
	evidence.Detail = nullStringPtr(detail)
	return &evidence, nil
}

func loadCandidateEvidenceTx(
	ctx context.Context, tx *loggedTx, candidateID int64,
) ([]IdentityMatchEvidence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT
		id, candidate_id, evidence_kind, evidence_ref, detail, source, created_at
		FROM identity_match_evidence WHERE candidate_id = ? ORDER BY id`, candidateID,
	)
	if err != nil {
		return nil, err
	}
	return scanIdentityMatchEvidenceRows(rows)
}

func scanIdentityMatchEvidenceRows(rows *loggedRows) ([]IdentityMatchEvidence, error) {
	defer func() { _ = rows.Close() }()
	evidence := make([]IdentityMatchEvidence, 0)
	for rows.Next() {
		var item IdentityMatchEvidence
		var evidenceRef, detail sql.NullString
		if err := rows.Scan(
			&item.ID, &item.CandidateID, &item.EvidenceKind,
			&evidenceRef, &detail, &item.Source, &item.CreatedAt,
		); err != nil {
			return nil, err
		}
		item.EvidenceRef = nullStringPtr(evidenceRef)
		item.Detail = nullStringPtr(detail)
		evidence = append(evidence, item)
	}
	return evidence, rows.Err()
}

func (s *Store) loadCandidateEvidencePageContext(
	ctx context.Context, candidates []IdentityMatchCandidate,
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
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, candidate_id, evidence_kind, evidence_ref, detail, source, created_at
		FROM identity_match_evidence WHERE candidate_id IN (`+
		strings.Join(placeholders, ",")+`) ORDER BY candidate_id, id`, args...)
	if err != nil {
		return fmt.Errorf("load identity match evidence: %w", err)
	}
	evidence, err := scanIdentityMatchEvidenceRows(rows)
	if err != nil {
		return fmt.Errorf("load identity match evidence: %w", err)
	}
	for _, item := range evidence {
		i := index[item.CandidateID]
		candidates[i].Evidence = append(candidates[i].Evidence, item)
	}
	return nil
}
