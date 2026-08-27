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
	IdentityMatchParticipant     IdentityMatchEndpointKind = "participant"
	IdentityMatchPerson          IdentityMatchEndpointKind = "person"
	IdentityMatchObservation     IdentityMatchEndpointKind = "observation"
	IdentityMatchContactPoint    IdentityMatchEndpointKind = "contact_point"
	IdentityMatchCardDAVResource IdentityMatchEndpointKind = "carddav_resource"
)

func (k IdentityMatchEndpointKind) valid() bool {
	switch k {
	case IdentityMatchParticipant, IdentityMatchPerson,
		IdentityMatchObservation, IdentityMatchContactPoint,
		IdentityMatchCardDAVResource:
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

type identityMatchConflictState struct {
	observationOrigin sql.NullString
	preConflictState  sql.NullString
}

type IdentityMatchCandidate struct {
	ID                 int64                     `json:"id"`
	LeftKind           IdentityMatchEndpointKind `json:"left_kind"`
	LeftID             int64                     `json:"left_id"`
	RightKind          IdentityMatchEndpointKind `json:"right_kind"`
	RightID            int64                     `json:"right_id"`
	Basis              IdentityMatchBasis        `json:"basis"`
	ServiceSlug        *string                   `json:"service_slug,omitempty"`
	ScopeKind          *string                   `json:"scope_kind,omitempty"`
	ScopeValue         *string                   `json:"scope_value,omitempty"`
	NormalizedValue    *string                   `json:"normalized_value,omitempty"`
	State              IdentityMatchState        `json:"state"`
	Confidence         *float64                  `json:"confidence,omitempty"`
	Source             Provenance                `json:"source"`
	SourceRef          *string                   `json:"source_ref,omitempty"`
	DecidedBy          *string                   `json:"decided_by,omitempty"`
	DecidedAt          *time.Time                `json:"decided_at,omitempty"`
	Notes              *string                   `json:"notes,omitempty"`
	Evidence           []IdentityMatchEvidence   `json:"evidence"`
	CreatedAt          time.Time                 `json:"created_at"`
	UpdatedAt          time.Time                 `json:"updated_at"`
	applicationPending bool
	conflictState      identityMatchConflictState
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
	// CrossScope marks a review candidate that intentionally compares the same
	// service-scoped username across different scopes. It keeps the candidate
	// itself unscoped while retaining the service namespace; it never permits
	// automatic acceptance.
	CrossScope bool
	State      IdentityMatchState
	Confidence *float64
	Source     Provenance
	SourceRef  *string
	// SourceID records the archive source that supports this generated
	// candidate. It is kept in a separate many-to-many table because one
	// candidate can be corroborated by several sources.
	SourceID *int64
	Notes    *string
}

type IdentityMatchEvidenceInput struct {
	EvidenceKind string
	EvidenceRef  *string
	Detail       *string
	Source       Provenance
	// SourceID records the archive source that supports this generated
	// evidence. Multiple sources may support one evidence row.
	SourceID *int64
}

var (
	ErrInvalidIdentityMatchEndpoint  = errors.New("invalid identity match endpoint kind")
	ErrInvalidIdentityMatchBasis     = errors.New("invalid identity match basis")
	ErrInvalidIdentityMatchState     = errors.New("invalid identity match state")
	ErrIdentityMatchSelfLink         = errors.New("identity match endpoints must differ")
	ErrIdentityMatchNotFound         = errors.New("identity match candidate not found")
	ErrIdentityMatchEndpointNotFound = errors.New("identity match endpoint not found")
	ErrIdentityMatchNotAcceptable    = errors.New("a username-only match requires stable provider corroboration or explicit confirmation")
	ErrIdentityMatchRejected         = errors.New("identity match candidate was rejected")
	ErrIdentityMatchAlreadyAccepted  = errors.New("an accepted identity match candidate cannot be rejected")
	ErrIdentityMatchAlreadyApplied   = errors.New("an applied identity match candidate cannot leave accepted state")
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
	input.ScopeKind = trimmedOrNil(input.ScopeKind)
	input.ScopeValue = trimmedOrNil(input.ScopeValue)
	input.NormalizedValue = trimmedOrNil(input.NormalizedValue)
	service, hasService, err := s.resolveOptionalCommunicationServiceContext(ctx, input.ServiceSlug)
	if err != nil {
		return nil, false, err
	}
	allowCrossScope := input.CrossScope && hasService &&
		input.Basis == IdentityMatchServiceScopeUsername &&
		input.ScopeKind == nil && input.ScopeValue == nil
	if err := ValidateServiceScope(service, input.ScopeKind, input.ScopeValue); err != nil &&
		!allowCrossScope {
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
		table = personContactPointsTableName
	case IdentityMatchCardDAVResource:
		table = "carddav_resources"
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
		if err := s.recordIdentityMatchCandidateSourceTx(
			ctx, tx, candidate.ID, input.SourceID,
		); err != nil {
			return nil, false, err
		}
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
	if err != nil {
		return nil, false, err
	}
	if err := s.recordIdentityMatchCandidateSourceTx(
		ctx, tx, id, input.SourceID,
	); err != nil {
		return nil, false, err
	}
	return candidate, true, nil
}

func (s *Store) recordIdentityMatchCandidateSourceTx(
	ctx context.Context, tx *loggedTx, candidateID int64, sourceID *int64,
) error {
	if sourceID == nil || *sourceID == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity_match_candidate_sources
			(candidate_id, source_id, is_conservative) VALUES (?, ?, FALSE)
		ON CONFLICT (candidate_id, source_id) DO UPDATE
		SET is_conservative = FALSE`, candidateID, *sourceID); err != nil {
		return fmt.Errorf("record identity match candidate source: %w", err)
	}
	return nil
}

// AttachIdentityMatchCandidateSourceContext records another archive source
// supporting an existing generated candidate. A candidate can be corroborated
// by observations from several sources, so source removal only drops it after
// its final support disappears.
func (s *Store) AttachIdentityMatchCandidateSourceContext(
	ctx context.Context, candidateID, sourceID int64,
) error {
	if candidateID == 0 || sourceID == 0 {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identity_match_candidate_sources
				(candidate_id, source_id, is_conservative) VALUES (?, ?, FALSE)
			ON CONFLICT (candidate_id, source_id) DO UPDATE
			SET is_conservative = FALSE`, candidateID, sourceID); err != nil {
			return fmt.Errorf("attach identity match candidate source: %w", err)
		}
		return nil
	})
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
		evidenceRef := stringValue(input.EvidenceRef)
		detail := stringValue(input.Detail)
		err := tx.QueryRowContext(ctx, `SELECT id FROM identity_match_evidence
			WHERE candidate_id = ? AND evidence_kind = ?
			  AND evidence_ref IS NOT DISTINCT FROM ?
			  AND detail IS NOT DISTINCT FROM ?
			  AND source = ?
			ORDER BY id LIMIT 1`,
			candidateID, kind, evidenceRef, detail, input.Source,
		).Scan(&id)
		inserted := false
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRowContext(ctx, `INSERT INTO identity_match_evidence (
				candidate_id, evidence_kind, evidence_ref, detail, source
			) VALUES (?, ?, ?, ?, ?) RETURNING id`,
				candidateID, kind, evidenceRef, detail, input.Source,
			).Scan(&id); err != nil {
				return fmt.Errorf("add identity match evidence: %w", err)
			}
			inserted = true
		} else if err != nil {
			return fmt.Errorf("find matching identity match evidence: %w", err)
		}
		if input.SourceID != nil && *input.SourceID != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO identity_match_evidence_sources
					(evidence_id, source_id, is_conservative) VALUES (?, ?, FALSE)
				ON CONFLICT (evidence_id, source_id) DO UPDATE
				SET is_conservative = FALSE`, id, *input.SourceID); err != nil {
				return fmt.Errorf("record identity match evidence source: %w", err)
			}
		}
		if inserted {
			if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates
				SET updated_at = `+s.dialect.Now()+` WHERE id = ?`, candidateID,
			); err != nil {
				return fmt.Errorf("touch identity match candidate: %w", err)
			}
		}
		evidence, err = getIdentityMatchEvidenceTx(ctx, tx, id)
		return err
	})
	return evidence, err
}

// AttachIdentityMatchEvidenceSourceContext records another archive source
// supporting an existing generated evidence row. Evidence can be corroborated
// by multiple sources, so source removal only drops the row after its final
// support disappears.
func (s *Store) AttachIdentityMatchEvidenceSourceContext(
	ctx context.Context, evidenceID, sourceID int64,
) error {
	if evidenceID == 0 || sourceID == 0 {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM identity_match_evidence WHERE id = ?`, evidenceID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check identity match evidence: %w", err)
		}
		if exists == 0 {
			return ErrIdentityMatchNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identity_match_evidence_sources
				(evidence_id, source_id, is_conservative) VALUES (?, ?, FALSE)
			ON CONFLICT (evidence_id, source_id) DO UPDATE
			SET is_conservative = FALSE`, evidenceID, sourceID); err != nil {
			return fmt.Errorf("attach identity match evidence source: %w", err)
		}
		return nil
	})
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
	return s.queryIdentityMatchCandidatesContext(ctx, query, args...)
}

func (s *Store) identityMatchCandidatesByIDContext(
	ctx context.Context, candidateIDs []int64,
) ([]IdentityMatchCandidate, error) {
	if len(candidateIDs) == 0 {
		return []IdentityMatchCandidate{}, nil
	}
	placeholders := make([]string, len(candidateIDs))
	args := make([]any, len(candidateIDs))
	for i, id := range candidateIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	return s.queryIdentityMatchCandidatesContext(ctx,
		identityMatchCandidateSelect+` WHERE c.id IN (`+
			strings.Join(placeholders, ",")+`) ORDER BY c.id`,
		args...,
	)
}

func (s *Store) queryIdentityMatchCandidatesContext(
	ctx context.Context, query string, args ...any,
) ([]IdentityMatchCandidate, error) {
	candidates, err := s.queryIdentityMatchCandidateRowsContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := s.loadCandidateEvidencePageContext(ctx, candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

// listPendingAcceptedIdentityMatchCandidatesContext reads only recovery work
// and deliberately leaves Evidence unloaded. Applying an already-decided
// participant assertion does not consult its explanatory evidence.
func (s *Store) listPendingAcceptedIdentityMatchCandidatesContext(
	ctx context.Context, limit int,
) ([]IdentityMatchCandidate, error) {
	limit = observationLookupLimit(limit)
	return s.queryIdentityMatchCandidateRowsContext(ctx,
		identityMatchCandidateSelect+` WHERE c.state = ?
			AND c.application_pending = TRUE
			AND c.left_kind = ? AND c.right_kind = ?
			ORDER BY c.id LIMIT ?`,
		IdentityMatchStateAccepted,
		IdentityMatchParticipant, IdentityMatchParticipant, limit,
	)
}

func (s *Store) queryIdentityMatchCandidateRowsContext(
	ctx context.Context, query string, args ...any,
) ([]IdentityMatchCandidate, error) {
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
	return candidates, nil
}

func (s *Store) DecideIdentityMatchCandidateContext(
	ctx context.Context,
	candidateID int64,
	state IdentityMatchState,
	decidedBy string,
	notes *string,
) (*IdentityMatchCandidate, error) {
	candidate, _, err := s.decideIdentityMatchCandidateContext(
		ctx, candidateID, state, decidedBy, notes)
	return candidate, err
}

func (s *Store) decideIdentityMatchCandidateContext(
	ctx context.Context,
	candidateID int64,
	state IdentityMatchState,
	decidedBy string,
	notes *string,
) (*IdentityMatchCandidate, *IdentityMatchCandidate, error) {
	if !state.valid() {
		return nil, nil, ErrInvalidIdentityMatchState
	}
	var candidate *IdentityMatchCandidate
	var before *IdentityMatchCandidate
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := getIdentityMatchCandidateTx(ctx, tx, candidateID)
		if err != nil {
			return err
		}
		before = current
		if current.State == IdentityMatchStateAccepted && state == IdentityMatchStateRejected {
			if current.DecidedBy != nil && *current.DecidedBy == string(ProvenanceUser) {
				return ErrIdentityMatchAlreadyAccepted
			}
			if current.LeftKind == IdentityMatchParticipant &&
				current.RightKind == IdentityMatchParticipant {
				if err := s.rejectSystemAcceptedIdentityMatchTxContext(
					ctx, tx, current.LeftID, current.RightID, current.ID,
				); err != nil {
					return err
				}
			}
		}
		// A system decision is a compare-and-set from the reviewable candidate
		// state. In particular, a user rejection that wins the identity lock
		// must not be overwritten by a stale importer snapshot. Explicit users
		// may still reverse their own rejection through the accept endpoint.
		if state == IdentityMatchStateAccepted && decidedBy != string(ProvenanceUser) &&
			current.State == IdentityMatchStateRejected {
			return ErrIdentityMatchRejected
		}
		if state == IdentityMatchStateAccepted && decidedBy != string(ProvenanceUser) &&
			current.State != IdentityMatchStateCandidate {
			// System acceptance is a compare-and-set from the reviewable
			// candidate state. Conflict and every other terminal state must
			// survive a stale importer snapshot just like rejection does.
			return ErrIdentityMatchNotAccepted
		}
		if current.State == IdentityMatchStateAccepted && state != IdentityMatchStateAccepted &&
			state != IdentityMatchStateRejected &&
			current.LeftKind == IdentityMatchParticipant &&
			current.RightKind == IdentityMatchParticipant {
			edges, err := s.loadLinkEdgesTxContext(ctx, tx)
			if err != nil {
				return err
			}
			if _, linked := componentOf(current.LeftID, edges)[current.RightID]; linked {
				return ErrIdentityMatchAlreadyApplied
			}
		}
		// Only a stable-provider-id candidate that records which stable ID
		// matched may be accepted without explicit user confirmation; the basis
		// label alone is caller-supplied and proves nothing.
		if state == IdentityMatchStateAccepted && decidedBy != string(ProvenanceUser) &&
			(current.Basis != IdentityMatchStableProviderID ||
				current.NormalizedValue == nil ||
				strings.TrimSpace(*current.NormalizedValue) == "") {
			return ErrIdentityMatchNotAcceptable
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates SET
			state = ?, decided_by = ?, decided_at = `+s.dialect.Now()+`,
			notes = ?, pre_conflict_state = NULL,
			application_pending = ?,
			observation_conflict_origin = CASE WHEN ?
				THEN NULL ELSE observation_conflict_origin END,
			updated_at = `+s.dialect.Now()+` WHERE id = ?`,
			state, decidedBy, stringValue(notes), state == IdentityMatchStateAccepted,
			state == IdentityMatchStateConflict && decidedBy == string(ProvenanceUser), candidateID,
		); err != nil {
			return fmt.Errorf("decide identity match candidate: %w", err)
		}
		candidate, err = getIdentityMatchCandidateTx(ctx, tx, candidateID)
		return err
	})
	return candidate, before, err
}

// rejectSystemAcceptedIdentityMatchTxContext withdraws the direct edge that
// was created for one automated match, if no other accepted match still
// supports it. The caller already holds the identity-mutation lock. Ownership
// transfers to another system-accepted candidate, or becomes manual when a
// user-accepted candidate protects the pair.
func (s *Store) rejectSystemAcceptedIdentityMatchTxContext(
	ctx context.Context, tx *loggedTx, leftID, rightID, candidateID int64,
) error {
	originalEdges, err := s.loadLinkEdgesTxContext(ctx, tx)
	if err != nil {
		return err
	}
	lo, hi := normalizeEdge(leftID, rightID)
	var replacementID int64
	var decidedBy sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, decided_by
		FROM identity_match_candidates
		WHERE id <> ? AND state = ?
		  AND left_kind = ? AND left_id = ?
		  AND right_kind = ? AND right_id = ?
		ORDER BY CASE WHEN decided_by = 'user' THEN 0 ELSE 1 END, id
		LIMIT 1`,
		candidateID, IdentityMatchStateAccepted,
		IdentityMatchParticipant, lo, IdentityMatchParticipant, hi,
	).Scan(&replacementID, &decidedBy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("find replacement identity match link owner: %w", err)
	}

	var result sql.Result
	if err == nil && decidedBy.String == string(ProvenanceUser) {
		result, err = tx.ExecContext(ctx, `UPDATE participant_links
			SET identity_match_candidate_id = NULL
			WHERE participant_a = ? AND participant_b = ?
			  AND identity_match_candidate_id = ?`, lo, hi, candidateID)
	} else if err == nil {
		result, err = tx.ExecContext(ctx, `UPDATE participant_links
			SET identity_match_candidate_id = ?
			WHERE participant_a = ? AND participant_b = ?
			  AND identity_match_candidate_id = ?`, replacementID, lo, hi, candidateID)
	} else {
		result, err = tx.ExecContext(ctx, `DELETE FROM participant_links
			WHERE participant_a = ? AND participant_b = ?
			  AND identity_match_candidate_id = ?`, lo, hi, candidateID)
	}
	if err != nil {
		return fmt.Errorf("replace rejected identity match link owner: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count replaced identity match link owner: %w", err)
	}
	if changed == 0 {
		return nil
	}
	if err := s.reapplyAcceptedIdentityMatchAfterRejectionTxContext(
		ctx, tx, candidateID, originalEdges,
	); err != nil {
		return err
	}
	if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
		return fmt.Errorf("bump identity revision after identity match rejection: %w", err)
	}
	return nil
}

// reapplyAcceptedIdentityMatchAfterRejectionTxContext restores an accepted
// assertion that crossed the split made by withdrawing an owned link. The
// candidate being rejected is still accepted until the caller updates its
// decision row, so it must be excluded explicitly.
func (s *Store) reapplyAcceptedIdentityMatchAfterRejectionTxContext(
	ctx context.Context,
	tx *loggedTx,
	rejectedCandidateID int64,
	originalEdges []linkEdge,
) error {
	currentEdges, err := s.loadLinkEdgesTxContext(ctx, tx)
	if err != nil {
		return err
	}
	originalClusters := clustersFromEdges(originalEdges)
	currentClusters := clustersFromEdges(currentEdges)
	rows, err := tx.QueryContext(ctx, `SELECT id, left_id, right_id, decided_by
		FROM identity_match_candidates
		WHERE id <> ? AND state = ?
		  AND left_kind = ? AND right_kind = ?
		ORDER BY CASE WHEN decided_by = 'user' THEN 0 ELSE 1 END, id`,
		rejectedCandidateID, IdentityMatchStateAccepted,
		IdentityMatchParticipant, IdentityMatchParticipant,
	)
	if err != nil {
		return fmt.Errorf("list accepted identity matches after rejection: %w", err)
	}
	type acceptedPair struct {
		id, left, right int64
		decidedBy       sql.NullString
	}
	accepted := make([]acceptedPair, 0)
	for rows.Next() {
		var candidate acceptedPair
		if err := rows.Scan(
			&candidate.id, &candidate.left, &candidate.right, &candidate.decidedBy,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan accepted identity match after rejection: %w", err)
		}
		accepted = append(accepted, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate accepted identity matches after rejection: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close accepted identity matches after rejection: %w", err)
	}

	for _, candidate := range accepted {
		originalLeft, leftWasLinked := originalClusters[candidate.left]
		originalRight, rightWasLinked := originalClusters[candidate.right]
		if !leftWasLinked || !rightWasLinked || originalLeft != originalRight {
			continue
		}
		currentLeft, leftStillLinked := currentClusters[candidate.left]
		currentRight, rightStillLinked := currentClusters[candidate.right]
		if leftStillLinked && rightStillLinked && currentLeft == currentRight {
			continue
		}
		lo, hi := normalizeEdge(candidate.left, candidate.right)
		if candidate.decidedBy.String == string(ProvenanceUser) {
			_, err = tx.ExecContext(ctx, `INSERT INTO participant_links
				(participant_a, participant_b) VALUES (?, ?)`, lo, hi)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO participant_links
				(participant_a, participant_b, identity_match_candidate_id)
				VALUES (?, ?, ?)`, lo, hi, candidate.id)
		}
		if err != nil {
			return fmt.Errorf("reapply accepted identity match %d after rejection: %w",
				candidate.id, err)
		}
		// Removing one edge from the link forest creates exactly two components;
		// the first accepted assertion crossing that split restores the original
		// connectivity, so no further candidate can require a new edge.
		return nil
	}
	return nil
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
	PreConflictState          sql.NullString
	DecidedBy                 sql.NullString
	DecidedAt                 sql.NullTime
	Notes                     sql.NullString
}

const identityMatchCandidateMergeSelect = `SELECT
	id, left_kind, left_id, right_kind, right_id, basis, service_id,
	scope_kind, scope_value, normalized_value, state, confidence, source, source_ref,
	observation_conflict_origin, pre_conflict_state, decided_by, decided_at, notes
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
		&candidate.PreConflictState, &candidate.DecidedBy,
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

func recordIdentityMatchCandidateRedirectTx(
	ctx context.Context,
	tx *loggedTx,
	retiredCandidateID, survivingCandidateID int64,
	endpointsCollapsed bool,
) error {
	if retiredCandidateID <= 0 || endpointsCollapsed == (survivingCandidateID > 0) {
		return errors.New("invalid identity match candidate redirect")
	}
	var survivor any
	if survivingCandidateID > 0 {
		survivor = survivingCandidateID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidate_redirects
		SET surviving_candidate_id = ?, endpoints_collapsed = ?
		WHERE surviving_candidate_id = ?`,
		survivor, endpointsCollapsed, retiredCandidateID,
	); err != nil {
		return fmt.Errorf("repoint identity match candidate redirects: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identity_match_candidate_redirects (
		retired_candidate_id, surviving_candidate_id, endpoints_collapsed
	) VALUES (?, ?, ?)
	ON CONFLICT (retired_candidate_id) DO UPDATE SET
		surviving_candidate_id = excluded.surviving_candidate_id,
		endpoints_collapsed = excluded.endpoints_collapsed`,
		retiredCandidateID, survivor, endpointsCollapsed,
	); err != nil {
		return fmt.Errorf("record identity match candidate redirect: %w", err)
	}
	return nil
}

func identityMatchCandidateRedirectTx(
	ctx context.Context, tx *loggedTx, retiredCandidateID int64,
) (survivingCandidateID int64, endpointsCollapsed, found bool, err error) {
	var survivor sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT
		surviving_candidate_id, endpoints_collapsed
		FROM identity_match_candidate_redirects
		WHERE retired_candidate_id = ?`, retiredCandidateID,
	).Scan(&survivor, &endpointsCollapsed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false,
			fmt.Errorf("load identity match candidate redirect: %w", err)
	}
	if endpointsCollapsed {
		return 0, true, true, nil
	}
	if !survivor.Valid || survivor.Int64 <= 0 {
		return 0, false, false, errors.New("identity match candidate redirect has no survivor")
	}
	return survivor.Int64, false, true, nil
}

func (s *Store) rewriteIdentityMatchCandidatesForMergeTx(
	ctx context.Context, tx *loggedTx, oldID, newID int64, edges []linkEdge,
) error {
	return s.rewriteIdentityMatchCandidatesForEndpointMergeTx(
		ctx, tx, IdentityMatchParticipant, oldID, newID, edges, false,
	)
}

func (s *Store) rewriteIdentityMatchCandidatesForEndpointMergeTx(
	ctx context.Context,
	tx *loggedTx,
	kind IdentityMatchEndpointKind,
	oldID, newID int64,
	edges []linkEdge,
	preferExisting bool,
) error {
	// Participant endpoint merges contract oldID into newID after candidate
	// reconciliation. Add that virtual edge now so an accepted survivor-side
	// collision sees the connectivity that the same transaction will retain.
	contractedEdges := make([]linkEdge, 0, len(edges)+1)
	contractedEdges = append(contractedEdges, edges...)
	contractedEdges = append(contractedEdges, linkEdge{a: oldID, b: newID})
	linkAdjacency := buildAdjacency(contractedEdges)
	rows, err := tx.QueryContext(ctx, identityMatchCandidateMergeSelect+`
		WHERE (left_kind = ? AND left_id = ?)
		   OR (right_kind = ? AND right_id = ?)
		ORDER BY id`+s.dialect.SelectForUpdate(),
		kind, oldID, kind, oldID,
	)
	if err != nil {
		return fmt.Errorf("load identity match candidates for endpoint merge: %w", err)
	}
	candidates, err := scanIdentityMatchCandidateMergeRows(rows)
	if err != nil {
		return fmt.Errorf("scan identity match candidates for endpoint merge: %w", err)
	}

	for _, candidate := range candidates {
		appliedAccepted := acceptedMergeCandidateIsLinked(candidate, linkAdjacency)
		if candidate.LeftKind == kind && candidate.LeftID == oldID {
			candidate.LeftID = newID
		}
		if candidate.RightKind == kind && candidate.RightID == oldID {
			candidate.RightID = newID
		}
		leftKind, leftID, rightKind, rightID, canonicalErr := canonicalMatchEndpoints(
			candidate.LeftKind, candidate.LeftID, candidate.RightKind, candidate.RightID,
		)
		if errors.Is(canonicalErr, ErrIdentityMatchSelfLink) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE participant_links SET identity_match_candidate_id = NULL
				 WHERE identity_match_candidate_id = ?`, candidate.ID); err != nil {
				return fmt.Errorf("clear owner of self-link candidate: %w", err)
			}
			if err := recordIdentityMatchCandidateRedirectTx(
				ctx, tx, candidate.ID, 0, true,
			); err != nil {
				return err
			}
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
		for _, collision := range collisions {
			appliedAccepted = appliedAccepted ||
				acceptedMergeCandidateIsLinked(collision, linkAdjacency)
		}
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

		preferredID := int64(0)
		if preferExisting {
			preferredID = collisions[0].ID
		}
		conflictNote := "participant merge reconciled opposing identity decisions"
		if kind == IdentityMatchPerson {
			conflictNote = "person merge reconciled opposing identity decisions"
		}
		if err := s.collapseIdentityMatchCandidateMergeGroupTx(
			ctx, tx, group, appliedAccepted, preferredID, conflictNote,
		); err != nil {
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
	ctx context.Context,
	tx *loggedTx,
	group []identityMatchCandidateMergeRow,
	appliedAccepted bool,
	preferredID int64,
	conflictNote string,
) error {
	sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
	if preferredID > 0 {
		for index := range group {
			if group[index].ID == preferredID {
				group[0], group[index] = group[index], group[0]
				break
			}
		}
	}
	winner := group[0]
	state, decidedBy, decidedAt, notes := reconcileIdentityMatchCandidateMergeState(
		group, appliedAccepted, conflictNote)
	confidence, source, sourceRef := identityMatchCandidateMergeConfidenceProvenance(group)
	observationOrigin := reconcileIdentityMatchCandidateMergeObservationOrigin(group, state)
	preConflict := reconcileIdentityMatchCandidateMergePreConflictState(group, state)

	for _, loser := range group[1:] {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identity_match_candidate_sources
				(candidate_id, source_id, is_conservative)
			SELECT ?, source_id, is_conservative
			FROM identity_match_candidate_sources
			WHERE candidate_id = ?
			ON CONFLICT (candidate_id, source_id) DO UPDATE
			SET is_conservative =
				identity_match_candidate_sources.is_conservative AND excluded.is_conservative`,
			winner.ID, loser.ID); err != nil {
			return fmt.Errorf("move identity match candidate source support: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_match_evidence
			SET candidate_id = ? WHERE candidate_id = ?`, winner.ID, loser.ID,
		); err != nil {
			return fmt.Errorf("move identity match candidate evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE participant_links
			SET identity_match_candidate_id = ?
			WHERE identity_match_candidate_id = ?`, winner.ID, loser.ID); err != nil {
			return fmt.Errorf("move identity match link ownership: %w", err)
		}
		if err := recordIdentityMatchCandidateRedirectTx(
			ctx, tx, loser.ID, winner.ID, false,
		); err != nil {
			return err
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
		observation_conflict_origin = ?, pre_conflict_state = ?,
		decided_by = ?, decided_at = ?, notes = ?, application_pending = ?,
		updated_at = `+s.dialect.Now()+` WHERE id = ?`,
		winner.LeftKind, winner.LeftID, winner.RightKind, winner.RightID, state,
		confidence, source, sourceRef, observationOrigin, preConflict,
		decidedBy, decidedAt, notes,
		state == IdentityMatchStateAccepted && !appliedAccepted, winner.ID,
	); err != nil {
		return fmt.Errorf("reconcile duplicate identity match candidate: %w", err)
	}
	return nil
}

// reconcileIdentityMatchCandidateMergePreConflictState records which state a
// collapsed conflict should return to once its observation support is gone.
// A terminal decision that lost to a conflict during the collapse (or a
// pre-conflict state a group member already carried) is restorable; opposing
// decisions cancel out and fall back to an undecided candidate.
func reconcileIdentityMatchCandidateMergePreConflictState(
	group []identityMatchCandidateMergeRow,
	state IdentityMatchState,
) sql.NullString {
	if state != IdentityMatchStateConflict {
		return sql.NullString{}
	}
	restorable := sql.NullString{}
	for _, candidate := range group {
		value := ""
		switch {
		case candidate.State == IdentityMatchStateAccepted ||
			candidate.State == IdentityMatchStateRejected:
			value = string(candidate.State)
		case candidate.PreConflictState.Valid:
			value = candidate.PreConflictState.String
		}
		if value == "" || value == string(IdentityMatchStateCandidate) {
			continue
		}
		if restorable.Valid && restorable.String != value {
			return sql.NullString{}
		}
		restorable = sql.NullString{String: value, Valid: true}
	}
	return restorable
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
		if candidate.State == IdentityMatchStateConflict {
			if !candidate.ObservationConflictOrigin.Valid {
				return sql.NullString{}
			}
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
	group []identityMatchCandidateMergeRow, appliedAccepted bool, conflictNote string,
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
	if appliedAccepted && hasAccepted {
		if decidedBy, decidedAt, notes, ok := preferredMergeDecisionMetadata(
			group, IdentityMatchStateAccepted,
		); ok {
			return IdentityMatchStateAccepted, decidedBy, decidedAt, notes
		}
		return IdentityMatchStateAccepted,
			sql.NullString{}, sql.NullTime{}, sql.NullString{}
	}
	generatedConflict := state != IdentityMatchStateConflict && hasAccepted && hasRejected
	if generatedConflict {
		return IdentityMatchStateConflict,
			sql.NullString{String: string(AttributeOwnershipSystem), Valid: true},
			sql.NullTime{Time: time.Now().UTC(), Valid: true},
			sql.NullString{
				String: conflictNote,
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
	if decidedBy, decidedAt, notes, ok := preferredMergeDecisionMetadata(
		group, state,
	); ok {
		return state, decidedBy, decidedAt, notes
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

func preferredMergeDecisionMetadata(
	group []identityMatchCandidateMergeRow,
	state IdentityMatchState,
) (sql.NullString, sql.NullTime, sql.NullString, bool) {
	for _, requireUser := range []bool{true, false} {
		for _, candidate := range group {
			if candidate.State != state || !hasMergeDecisionMetadata(candidate) {
				continue
			}
			isUser := candidate.DecidedBy.Valid &&
				candidate.DecidedBy.String == string(ProvenanceUser)
			if requireUser != isUser {
				continue
			}
			return candidate.DecidedBy, candidate.DecidedAt, candidate.Notes, true
		}
	}
	return sql.NullString{}, sql.NullTime{}, sql.NullString{}, false
}

func hasMergeDecisionMetadata(candidate identityMatchCandidateMergeRow) bool {
	return candidate.DecidedBy.Valid || candidate.DecidedAt.Valid || candidate.Notes.Valid
}

func acceptedMergeCandidateIsLinked(
	candidate identityMatchCandidateMergeRow,
	linkAdjacency map[int64][]int64,
) bool {
	if candidate.State != IdentityMatchStateAccepted ||
		candidate.LeftKind != IdentityMatchParticipant ||
		candidate.RightKind != IdentityMatchParticipant {
		return false
	}
	_, linked := componentOfAdj(candidate.LeftID, linkAdjacency)[candidate.RightID]
	return linked
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
	c.notes, c.created_at, c.updated_at, c.application_pending,
	c.observation_conflict_origin, c.pre_conflict_state
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
	candidate, err := getIdentityMatchCandidateWithoutEvidenceTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	candidate.Evidence, err = loadCandidateEvidenceTx(ctx, tx, id)
	return candidate, err
}

func getIdentityMatchCandidateWithoutEvidenceTx(
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
	return candidate, nil
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
		&candidate.applicationPending, &candidate.conflictState.observationOrigin,
		&candidate.conflictState.preConflictState,
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
