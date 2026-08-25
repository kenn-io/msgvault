package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

var ErrPersonFactPersonNotTracked = errors.New("person is not tracked for automatic fact maintenance")

type personFactTxRunner func(context.Context, func(*loggedTx) error) error

type personFactTargetProjector interface {
	loadCurrent(ctx context.Context, personID int64, target personfacts.TargetDescriptor) ([]personfacts.CurrentProjection, error)
	projectionContext(ctx context.Context, personID int64, claims []personfacts.ResolvedClaim) (string, error)
	project(ctx context.Context, plan personfacts.ProjectionPlan, claim personfacts.ResolvedClaim,
		decisionKey string, transactionTime time.Time) (*personfacts.ProjectionRef, bool, error)
}

type personFactAttributeProjector struct {
	store      *Store
	tx         *loggedTx
	definition AttributeDefinition
}

type personFactEmploymentProjector struct {
	store             *Store
	tx                *loggedTx
	lock              bool
	organizationLocks *personFactOrganizationLockSet
	consumed          map[int64]struct{}
}

type personFactRetiredProjection struct {
	Ref      personfacts.ProjectionRef
	ClaimKey string
}

const personFactDecisionSourceRefPrefix = "person-fact-decision:"
const personFactProjectionKindEmployment = "employment"

type personFactTouchedTarget struct {
	Kind            personfacts.TargetKind
	Key             string
	Fallback        *personfacts.TargetDescriptor
	FallbackClaimID int64
}

type personFactTargetEligibility struct {
	Supported              bool
	SensitivePolicyBlocked bool
}

type personFactClaimChronology struct {
	ResolvedAt   time.Time
	GenerationID int64
}

func (s *Store) ApplyPersonFactGenerationContext(
	ctx context.Context, input personfacts.GenerationInput, aligner personfacts.EvidenceAligner,
) (*personfacts.GenerationResult, error) {
	prepared, err := personfacts.PreparePersonFactGeneration(ctx, input, aligner)
	if err != nil {
		return nil, err
	}
	return retryContendedWrite(ctx, s, "apply person fact generation",
		func() (*personfacts.GenerationResult, error) {
			var result *personfacts.GenerationResult
			err := s.withTxContext(ctx, func(tx *loggedTx) error {
				var applyErr error
				result, applyErr = s.applyPreparedPersonFactGenerationTx(ctx, tx, prepared)
				return applyErr
			})
			return result, err
		})
}

func (s *Store) applyPersonFactGenerationContext(
	ctx context.Context, input personfacts.GenerationInput, aligner personfacts.EvidenceAligner,
	runTx personFactTxRunner,
) (*personfacts.GenerationResult, error) {
	prepared, err := personfacts.PreparePersonFactGeneration(ctx, input, aligner)
	if err != nil {
		return nil, err
	}
	if runTx == nil {
		return nil, errors.New("person fact transaction runner is required")
	}
	var result *personfacts.GenerationResult
	err = runTx(ctx, func(tx *loggedTx) error {
		var applyErr error
		result, applyErr = s.applyPreparedPersonFactGenerationTx(ctx, tx, prepared)
		return applyErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) applyPreparedPersonFactGenerationTx(
	ctx context.Context, tx *loggedTx, prepared personfacts.PreparedGeneration,
) (*personfacts.GenerationResult, error) {
	input, claims, statuses, err := verifyPreparedPersonFactGeneration(prepared)
	if err != nil {
		return nil, err
	}
	if err := s.lockProfileIdentityKeyTxContext(
		ctx, tx, "person-fact-generation", input.PersonID); err != nil {
		return nil, err
	}
	if err := verifyTrackedPersonFactPersonTx(ctx, tx, input.PersonID); err != nil {
		return nil, err
	}
	touched, err := s.personFactTouchedTargetsTx(ctx, tx, input.PersonID, claims, statuses)
	if err != nil {
		return nil, err
	}
	for _, target := range touched {
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "person-fact-target", input.PersonID, target.Kind, target.Key); err != nil {
			return nil, err
		}
	}
	organizationRefs, employmentTouched, err := s.personFactGenerationEmploymentOrganizationReferencesTx(
		ctx, tx, input.PersonID, touched, claims)
	if err != nil {
		return nil, err
	}
	if employmentTouched {
		if err := s.lockPersonFactOrganizationTableForReferencesTx(
			ctx, tx, organizationRefs); err != nil {
			return nil, err
		}
	}
	for _, target := range touched {
		if target.Kind == personfacts.TargetEmployment {
			if err := s.lockEmploymentPeopleTx(ctx, tx, input.PersonID); err != nil {
				return nil, err
			}
			break
		}
	}

	generation, replay, err := s.insertPersonFactGenerationTx(ctx, tx, prepared)
	if err != nil {
		return nil, err
	}
	if replay {
		return s.loadPersonFactGenerationResultTx(ctx, tx, input.PersonID, generation.GenerationKey)
	}

	preparedFailures := make(map[string]*personfacts.ValidationFailure, len(claims))
	for index := range claims {
		claimKey, keyErr := personfacts.ClaimKey(generation.GenerationKey, claims[index])
		if keyErr != nil {
			return nil, keyErr
		}
		failure := claims[index].Failure
		descriptor, eligibility, loadErr := s.loadPersonFactTargetDescriptorTx(
			ctx, tx, claims[index].Target.Kind, claims[index].Target.Key,
			&claims[index].Target, input.Policy.AllowSensitive)
		if loadErr != nil {
			return nil, loadErr
		}
		if failure == nil && !eligibility.Supported {
			failure = &personfacts.ValidationFailure{
				Action: personfacts.DecisionInvalid, Reason: personfacts.ReasonUnsupportedTarget,
				Detail: "target is no longer eligible for automatic projection",
			}
		} else if failure == nil && descriptor.Revision != claims[index].Target.Revision {
			failure = &personfacts.ValidationFailure{
				Action: personfacts.DecisionInvalid, Reason: personfacts.ReasonStaleTargetRevision,
				Detail: "target descriptor changed after generation preparation",
			}
		}
		if failure != nil {
			copyFailure := *failure
			preparedFailures[claimKey] = &copyFailure
		}
	}
	var organizationLocks *personFactOrganizationLockSet
	if employmentTouched {
		organizationLocks, err = s.lockPersonFactOrganizationReferencesTx(
			ctx, tx, organizationRefs)
		if err != nil {
			return nil, err
		}
	}
	if err := s.preflightPersonFactEmploymentOrganizationsTx(
		ctx, tx, generation.GenerationKey, claims, preparedFailures,
		organizationLocks); err != nil {
		return nil, err
	}
	if _, err := s.insertPersonFactClaimsWithFailuresTx(
		ctx, tx, generation, claims, preparedFailures); err != nil {
		return nil, err
	}
	if err := s.insertPersonFactEvidenceStatusEventsTx(ctx, tx, generation, statuses); err != nil {
		return nil, err
	}

	transactionTime := time.Now().UTC()
	projectionChanged := false
	for _, touchedTarget := range touched {
		descriptor, eligibility, loadErr := s.loadPersonFactTargetDescriptorTx(
			ctx, tx, touchedTarget.Kind, touchedTarget.Key, touchedTarget.Fallback,
			input.Policy.AllowSensitive)
		if loadErr != nil {
			return nil, loadErr
		}
		changed, resolveErr := s.resolvePersonFactTargetWithOrganizationLocksTx(
			ctx, tx, generation, descriptor, eligibility, input.Policy,
			preparedFailures, organizationLocks, transactionTime)
		if resolveErr != nil {
			return nil, resolveErr
		}
		projectionChanged = projectionChanged || changed
	}
	if projectionChanged {
		if err := s.bumpPersonVCardProjectionsTx(ctx, tx, input.PersonID); err != nil {
			return nil, err
		}
	}
	return s.loadPersonFactGenerationResultTx(ctx, tx, input.PersonID, generation.GenerationKey)
}

func (s *Store) preflightPersonFactEmploymentOrganizationsTx(
	ctx context.Context, tx *loggedTx, generationKey string,
	claims []personfacts.PreparedClaim, failures map[string]*personfacts.ValidationFailure,
	organizationLocks *personFactOrganizationLockSet,
) error {
	type preparedReference struct {
		claimKey string
		ref      personfacts.OrganizationReference
		keys     []personFactOrganizationLookupKey
	}
	prepared := make([]preparedReference, 0, len(claims))
	for _, claim := range claims {
		if claim.Target.Kind != personfacts.TargetEmployment || claim.Normalized == nil {
			continue
		}
		claimKey, err := personfacts.ClaimKey(generationKey, claim)
		if err != nil {
			return err
		}
		if failures[claimKey] != nil {
			continue
		}
		var value personfacts.EmploymentValue
		if err := json.Unmarshal(claim.Normalized.JSON, &value); err != nil {
			return fmt.Errorf("decode normalized employment organization preflight: %w", err)
		}
		ref, keys, normalizeErr := normalizePersonFactOrganizationReference(value.Organization)
		if normalizeErr != nil {
			failure := personFactOrganizationReferenceFailure(normalizeErr)
			if failure == nil {
				return normalizeErr
			}
			failures[claimKey] = failure
			continue
		}
		prepared = append(prepared, preparedReference{claimKey: claimKey, ref: ref, keys: keys})
	}
	for _, reference := range prepared {
		_, err := s.prepareLockedPersonFactOrganizationReferenceTx(
			ctx, tx, reference.ref, reference.keys, organizationLocks, true)
		if err == nil {
			continue
		}
		failure := personFactOrganizationReferenceFailure(err)
		if failure == nil {
			return err
		}
		failures[reference.claimKey] = failure
	}
	return nil
}

func (s *Store) personFactGenerationEmploymentOrganizationReferencesTx(
	ctx context.Context, tx *loggedTx, personID int64,
	touched []personFactTouchedTarget, claims []personfacts.PreparedClaim,
) ([]personfacts.OrganizationReference, bool, error) {
	targetKeys := make(map[string]struct{})
	for _, target := range touched {
		if target.Kind == personfacts.TargetEmployment {
			targetKeys[target.Key] = struct{}{}
		}
	}
	if len(targetKeys) == 0 {
		return nil, false, nil
	}
	refs := make([]personfacts.OrganizationReference, 0, len(claims))
	for _, claim := range claims {
		if claim.Target.Kind != personfacts.TargetEmployment || claim.Normalized == nil {
			continue
		}
		if _, touchedTarget := targetKeys[claim.Target.Key]; !touchedTarget {
			continue
		}
		var value personfacts.EmploymentValue
		if err := json.Unmarshal(claim.Normalized.JSON, &value); err != nil {
			return nil, false, fmt.Errorf(
				"decode incoming employment organization lock scope: %w", err)
		}
		refs = append(refs, value.Organization)
	}
	historical, err := s.loadPersonFactEmploymentOrganizationReferencesTx(
		ctx, tx, personID, targetKeys)
	if err != nil {
		return nil, false, err
	}
	refs = append(refs, historical...)
	return refs, true, nil
}

func (s *Store) loadPersonFactEmploymentOrganizationReferencesTx(
	ctx context.Context, tx *loggedTx, personID int64, targetKeys map[string]struct{},
) ([]personfacts.OrganizationReference, error) {
	orderedTargetKeys := make([]string, 0, len(targetKeys))
	for targetKey := range targetKeys {
		orderedTargetKeys = append(orderedTargetKeys, targetKey)
	}
	sort.Strings(orderedTargetKeys)
	if len(orderedTargetKeys) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(orderedTargetKeys)+2)
	args = append(args, personID, personfacts.TargetEmployment)
	for _, targetKey := range orderedTargetKeys {
		args = append(args, targetKey)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.claim_key, CAST(c.normalized_value_json AS TEXT)
		FROM person_fact_claims c
		WHERE c.person_id = ? AND c.target_kind = ?
		  AND c.target_key IN (`+placeholders(len(orderedTargetKeys))+`)
		  AND c.normalized_value_json IS NOT NULL AND c.rejection_action IS NULL
		ORDER BY c.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list historical employment organization lock scope: %w", err)
	}
	type historicalReference struct {
		claimKey string
		ref      personfacts.OrganizationReference
	}
	historical := make([]historicalReference, 0)
	claimKeys := make([]string, 0)
	for rows.Next() {
		var claimKey, normalizedJSON string
		if scanErr := rows.Scan(&claimKey, &normalizedJSON); scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan historical employment organization lock scope: %w", scanErr)
		}
		var value personfacts.EmploymentValue
		if decodeErr := json.Unmarshal([]byte(normalizedJSON), &value); decodeErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode historical employment organization lock scope: %w", decodeErr)
		}
		historical = append(historical, historicalReference{claimKey: claimKey, ref: value.Organization})
		claimKeys = append(claimKeys, claimKey)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate historical employment organization lock scope: %w", rowsErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return nil, fmt.Errorf("close historical employment organization lock scope: %w", closeErr)
	}
	bindings, err := loadPersonFactEmploymentOrganizationBindingsTx(ctx, tx, personID, claimKeys)
	if err != nil {
		return nil, err
	}
	refs := make([]personfacts.OrganizationReference, 0, len(historical))
	for _, reference := range historical {
		if organizationID, bound := bindings[reference.claimKey]; bound {
			reference.ref.ID = &organizationID
		}
		refs = append(refs, reference.ref)
	}
	return refs, nil
}

func personFactOrganizationReferenceFailure(err error) *personfacts.ValidationFailure {
	if !errors.Is(err, ErrOrganizationNotFound) && !errors.Is(err, ErrOrganizationInvalid) {
		return nil
	}
	return &personfacts.ValidationFailure{
		Action: personfacts.DecisionInvalid, Reason: personfacts.ReasonMalformedValue,
		Detail: err.Error(),
	}
}

func verifyPreparedPersonFactGeneration(
	prepared personfacts.PreparedGeneration,
) (personfacts.GenerationInput, []personfacts.PreparedClaim, []personfacts.PreparedEvidenceStatusChange, error) {
	input := prepared.Input()
	claims := prepared.Claims()
	statuses := prepared.EvidenceStatusChanges()
	if input.PersonID <= 0 || len(prepared.CanonicalJSON()) == 0 || prepared.GenerationKey() == "" {
		return personfacts.GenerationInput{}, nil, nil, errors.New("prepared person fact generation is empty")
	}
	key, err := personfacts.GenerationKey(input, claims, statuses)
	if err != nil {
		return personfacts.GenerationInput{}, nil, nil, fmt.Errorf("verify prepared person fact generation: %w", err)
	}
	if key != prepared.GenerationKey() || input.ProgramFingerprint != prepared.ProgramFingerprint() {
		return personfacts.GenerationInput{}, nil, nil,
			errors.New("prepared person fact generation failed integrity verification")
	}
	return input, claims, statuses, nil
}

func verifyTrackedPersonFactPersonTx(ctx context.Context, tx *loggedTx, personID int64) error {
	var tracked int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM persons p
		WHERE p.id = ? AND EXISTS (
			SELECT 1 FROM person_tracking pt WHERE pt.person_id = p.id
		)`, personID).Scan(&tracked)
	if err != nil {
		return fmt.Errorf("verify tracked person %d: %w", personID, err)
	}
	if tracked == 0 {
		return ErrPersonFactPersonNotTracked
	}
	return nil
}

func (s *Store) personFactTouchedTargetsTx(
	ctx context.Context, tx *loggedTx, personID int64,
	claims []personfacts.PreparedClaim, statuses []personfacts.PreparedEvidenceStatusChange,
) ([]personFactTouchedTarget, error) {
	targets := make(map[string]personFactTouchedTarget)
	for index := range claims {
		target := claims[index].Target
		key := personFactTargetMapKey(target.Kind, target.Key)
		copyTarget := target
		targets[key] = personFactTouchedTarget{Kind: target.Kind, Key: target.Key, Fallback: &copyTarget}
	}
	for _, status := range statuses {
		var evidenceID int64
		var sourceVersion string
		err := tx.QueryRowContext(ctx, `
			SELECT id, source_version FROM person_fact_evidence
			WHERE person_id = ? AND evidence_key = ?`, personID, status.EvidenceKey,
		).Scan(&evidenceID, &sourceVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("person fact status references unknown evidence %q", status.EvidenceKey)
		}
		if err != nil {
			return nil, fmt.Errorf("verify person fact status evidence %q: %w", status.EvidenceKey, err)
		}
		if sourceVersion != status.SourceVersion {
			return nil, fmt.Errorf("person fact status source version does not match evidence %q", status.EvidenceKey)
		}
		rows, queryErr := tx.QueryContext(ctx, `
			SELECT c.id, c.target_kind, c.target_key, c.target_revision
			FROM person_fact_claim_evidence ce
			JOIN person_fact_claims c ON c.id = ce.claim_id
			WHERE ce.evidence_id = ? AND c.person_id = ?
			ORDER BY c.target_kind, c.target_key, c.id`, evidenceID, personID)
		if queryErr != nil {
			return nil, fmt.Errorf("discover person fact status targets: %w", queryErr)
		}
		for rows.Next() {
			var claimID int64
			var kind personfacts.TargetKind
			var key, revision string
			if scanErr := rows.Scan(&claimID, &kind, &key, &revision); scanErr != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan person fact status target: %w", scanErr)
			}
			mapKey := personFactTargetMapKey(kind, key)
			if existing, ok := targets[mapKey]; ok && existing.Fallback != nil &&
				(existing.FallbackClaimID == 0 || existing.FallbackClaimID >= claimID) {
				continue
			}
			fallback := personfacts.TargetDescriptor{
				Kind: kind, Key: key, UniversalID: key, Revision: revision,
				ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
			}
			if kind == personfacts.TargetEmployment {
				fallback.ValueType = personfacts.ValueEmployment
				fallback.Cardinality = personfacts.CardinalityMulti
			}
			targets[mapKey] = personFactTouchedTarget{
				Kind: kind, Key: key, Fallback: &fallback, FallbackClaimID: claimID,
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate person fact status targets: %w", rowsErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return nil, fmt.Errorf("close person fact status targets: %w", closeErr)
		}
	}
	result := make([]personFactTouchedTarget, 0, len(targets))
	for _, target := range targets {
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].Key < result[j].Key
	})
	return result, nil
}

func (s *Store) resolvePersonFactTargetTx(
	ctx context.Context, tx *loggedTx, generation personfacts.Generation,
	descriptor personfacts.TargetDescriptor, eligibility personFactTargetEligibility,
	policyContext personfacts.PolicyContext,
	preparedFailures map[string]*personfacts.ValidationFailure, transactionTime time.Time,
) (bool, error) {
	return s.resolvePersonFactTargetWithOrganizationLocksTx(
		ctx, tx, generation, descriptor, eligibility, policyContext,
		preparedFailures, nil, transactionTime)
}

func (s *Store) resolvePersonFactTargetWithOrganizationLocksTx(
	ctx context.Context, tx *loggedTx, generation personfacts.Generation,
	descriptor personfacts.TargetDescriptor, eligibility personFactTargetEligibility,
	policyContext personfacts.PolicyContext,
	preparedFailures map[string]*personfacts.ValidationFailure,
	organizationLocks *personFactOrganizationLockSet, transactionTime time.Time,
) (bool, error) {
	resolvedClaims, chronology, err := s.loadPersonFactResolvedClaimsTx(
		ctx, tx, generation.PersonID, descriptor, preparedFailures)
	if err != nil {
		return false, err
	}
	var projector personFactTargetProjector
	var current []personfacts.CurrentProjection
	var pin personfacts.PinState
	var projectionContext string
	supportedKind := descriptor.Kind == personfacts.TargetAttribute ||
		descriptor.Kind == personfacts.TargetEmployment
	projectable := eligibility.Supported && supportedKind
	if projectable {
		projector, err = s.personFactProjectorWithOrganizationLocksTx(
			ctx, tx, descriptor, organizationLocks)
		if err != nil {
			return false, err
		}
		current, err = projector.loadCurrent(ctx, generation.PersonID, descriptor)
		if err != nil {
			return false, err
		}
		pin, err = s.effectivePersonFactPinTx(ctx, tx, generation.PersonID, descriptor, current)
		if err != nil {
			return false, err
		}
		projectionContext, err = projector.projectionContext(ctx, generation.PersonID, resolvedClaims)
		if err != nil {
			return false, err
		}
	} else {
		for index := range resolvedClaims {
			resolvedClaims[index].Normalized = nil
			if resolvedClaims[index].Failure == nil {
				if eligibility.SensitivePolicyBlocked && supportedKind {
					resolvedClaims[index].Failure = &personfacts.ValidationFailure{
						Action: personfacts.DecisionPolicyRejected, Reason: personfacts.ReasonSensitivePolicy,
						Detail: "sensitive target is disabled by policy",
					}
				} else {
					resolvedClaims[index].Failure = &personfacts.ValidationFailure{
						Action: personfacts.DecisionInvalid, Reason: personfacts.ReasonUnsupportedTarget,
						Detail: "target is unavailable or ineligible for automatic projection",
					}
				}
			}
		}
	}
	resolverInputClaims := resolvedClaims
	if projectable && descriptor.Kind == personfacts.TargetEmployment {
		resolverInputClaims, err = s.reconcilePersonFactEmploymentClaimsTx(
			ctx, tx, generation.PersonID, resolvedClaims, chronology)
		if err != nil {
			return false, err
		}
	}
	resolver, err := personfacts.NewResolver(personfacts.DefaultPolicyV1())
	if err != nil {
		return false, err
	}
	resolvedAt, err := personFactTargetResolutionWatermarkTx(
		ctx, tx, generation.PersonID, descriptor, generation.ResolvedAt)
	if err != nil {
		return false, err
	}
	resolverClaims, ambiguousClaims := personFactResolverClaims(resolverInputClaims)
	resolution, err := resolver.Resolve(personfacts.ResolutionInput{
		PersonID: generation.PersonID, Target: descriptor, ResolvedAt: resolvedAt,
		Policy: policyContext, ProjectionContextFingerprint: projectionContext,
		Current: current, Claims: resolverClaims, Pin: pin,
	})
	if err != nil {
		return false, fmt.Errorf("resolve person fact target %s/%s: %w", descriptor.Kind, descriptor.Key, err)
	}
	if err := restorePersonFactAmbiguousDecisions(&resolution, ambiguousClaims); err != nil {
		return false, err
	}
	if projectable && descriptor.Kind == personfacts.TargetEmployment {
		if err := reconcilePersonFactEmploymentCompetition(&resolution, resolverInputClaims); err != nil {
			return false, err
		}
	}
	if !projectable && len(resolution.Projections) != 0 {
		return false, errors.New("unsupported person fact target produced a projection plan")
	}
	shell, err := s.insertPersonFactResolutionTx(ctx, tx, generation,
		personfacts.Resolution{
			Target: resolution.Target, ResolverVersion: resolution.ResolverVersion,
			InputFingerprint: resolution.InputFingerprint, ResolvedAt: resolution.ResolvedAt,
			Decisions: []personfacts.Decision{}, Projections: []personfacts.ProjectionPlan{},
		}, policyContext.ProviderPolicyFingerprint)
	if err != nil {
		return false, err
	}
	claimByKey := make(map[string]personfacts.ResolvedClaim, len(resolvedClaims))
	for _, claim := range resolvedClaims {
		claimByKey[claim.ClaimKey] = claim
	}
	decisionIndex := make(map[string]int, len(resolution.Decisions))
	for index := range resolution.Decisions {
		decisionIndex[resolution.Decisions[index].ClaimKey] = index
	}
	sortPersonFactProjectionPlans(resolution.Projections)
	changed := false
	plannedCurrent := make(map[personfacts.ProjectionRef]struct{})
	for _, plan := range resolution.Projections {
		if plan.CurrentRef != nil {
			plannedCurrent[*plan.CurrentRef] = struct{}{}
		}
		claim, ok := claimByKey[plan.ClaimKey]
		if !ok {
			return false, fmt.Errorf("projection plan references unknown claim %q", plan.ClaimKey)
		}
		index, ok := decisionIndex[plan.ClaimKey]
		if !ok {
			return false, fmt.Errorf("projection plan references missing decision %q", plan.ClaimKey)
		}
		ref, planChanged, projectErr := projector.project(
			ctx, plan, claim, resolution.Decisions[index].DecisionKey, transactionTime)
		if projectErr != nil {
			return false, projectErr
		}
		if ref != nil {
			resolution.Decisions[index].Projection = ref
		}
		changed = changed || planChanged
	}
	if attribute, ok := projector.(*personFactAttributeProjector); ok && !pin.Pinned {
		retired, retireChanged, retireErr := attribute.retireUnsupported(
			ctx, generation.PersonID, resolution.ResolvedAt, transactionTime, current, resolvedClaims,
			resolution.Decisions, plannedCurrent)
		if retireErr != nil {
			return false, retireErr
		}
		for _, projection := range retired {
			if index, exists := decisionIndex[projection.ClaimKey]; exists && resolution.Decisions[index].Projection == nil {
				ref := projection.Ref
				resolution.Decisions[index].Projection = &ref
			}
		}
		changed = changed || retireChanged
	}
	if employment, ok := projector.(*personFactEmploymentProjector); ok && !pin.Pinned {
		retired, retireChanged, retireErr := employment.retireExpired(
			ctx, generation.PersonID, resolution.ResolvedAt, current, resolvedClaims,
			resolution.Decisions, plannedCurrent)
		if retireErr != nil {
			return false, retireErr
		}
		for _, projection := range retired {
			if index, exists := decisionIndex[projection.ClaimKey]; exists && resolution.Decisions[index].Projection == nil {
				ref := projection.Ref
				resolution.Decisions[index].Projection = &ref
			}
		}
		changed = changed || retireChanged
	}
	for _, decision := range resolution.Decisions {
		if err := s.insertPersonFactDecisionTx(ctx, tx, generation.PersonID, shell.ID,
			resolution.InputFingerprint, decision); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func personFactTargetResolutionWatermarkTx(
	ctx context.Context, tx *loggedTx, personID int64, descriptor personfacts.TargetDescriptor,
	incoming time.Time,
) (time.Time, error) {
	var stored personFactTimestamp
	err := tx.QueryRowContext(ctx, `
		SELECT resolved_at FROM person_fact_resolutions
		WHERE person_id = ? AND target_kind = ? AND target_key = ?
		ORDER BY resolved_at DESC, id DESC LIMIT 1`,
		personID, descriptor.Kind, descriptor.Key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return incoming, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("load person fact target resolution watermark: %w", err)
	}
	if stored.Time.After(incoming) {
		return stored.Time, nil
	}
	return incoming, nil
}

func personFactResolverClaims(
	claims []personfacts.ResolvedClaim,
) ([]personfacts.ResolvedClaim, map[string]struct{}) {
	resolverClaims := append([]personfacts.ResolvedClaim(nil), claims...)
	ambiguous := make(map[string]struct{})
	for index := range resolverClaims {
		failure := resolverClaims[index].Failure
		if failure == nil || failure.Action != personfacts.DecisionAmbiguousRetained ||
			failure.Reason != personfacts.ReasonOrganizationAmbiguous {
			continue
		}
		ambiguous[resolverClaims[index].ClaimKey] = struct{}{}
		// Resolver v1's durable rejection input predates organization matching
		// and accepts only its original invalid/identity/policy vocabulary. Feed
		// it a non-eligible rejection so the ambiguous claim cannot compete or
		// project, then restore the exact organization decision below. The real
		// match identity is already bound into ProjectionContextFingerprint.
		resolverClaims[index].Failure = &personfacts.ValidationFailure{
			Action: personfacts.DecisionIdentityRejected,
			Reason: personfacts.ReasonIdentityMismatch,
			Detail: failure.Detail,
		}
	}
	return resolverClaims, ambiguous
}

func restorePersonFactAmbiguousDecisions(
	resolution *personfacts.Resolution, ambiguous map[string]struct{},
) error {
	if len(ambiguous) == 0 {
		return nil
	}
	for index := range resolution.Decisions {
		decision := &resolution.Decisions[index]
		if _, exists := ambiguous[decision.ClaimKey]; !exists {
			continue
		}
		decision.Action = personfacts.DecisionAmbiguousRetained
		decision.Reason = personfacts.ReasonOrganizationAmbiguous
		decision.CompetingClaimKey = ""
		decision.Projection = nil
		key, err := personfacts.DecisionKey(
			resolution.InputFingerprint, decision.ClaimKey, decision.Action)
		if err != nil {
			return err
		}
		decision.DecisionKey = key
	}
	plans := resolution.Projections[:0]
	for _, plan := range resolution.Projections {
		if _, exists := ambiguous[plan.ClaimKey]; !exists {
			plans = append(plans, plan)
		}
	}
	resolution.Projections = plans
	return nil
}

func (s *Store) loadPersonFactResolvedClaimsTx(
	ctx context.Context, tx *loggedTx, personID int64, descriptor personfacts.TargetDescriptor,
	preparedFailures map[string]*personfacts.ValidationFailure,
) ([]personfacts.ResolvedClaim, map[string]personFactClaimChronology, error) {
	claims, err := loadPersonFactTargetClaimsTx(ctx, tx, personID, descriptor)
	if err != nil {
		return nil, nil, err
	}
	chronologyByGeneration, err := loadPersonFactTargetChronologyTx(
		ctx, tx, personID, descriptor)
	if err != nil {
		return nil, nil, err
	}
	evidenceByClaim, err := loadPersonFactTargetEvidenceTx(ctx, tx, personID, descriptor)
	if err != nil {
		return nil, nil, err
	}
	statusByEvidence, err := loadPersonFactTargetEvidenceStatusesTx(ctx, tx, personID, descriptor)
	if err != nil {
		return nil, nil, err
	}

	resolved := make([]personfacts.ResolvedClaim, 0, len(claims))
	chronology := make(map[string]personFactClaimChronology, len(claims))
	for _, claim := range claims {
		proposedTarget := descriptor
		proposedTarget.Revision = claim.Target.Revision
		item := personfacts.ResolvedClaim{
			ClaimKey: claim.ClaimKey,
			Claim: personfacts.ProposedClaim{
				Target: proposedTarget, Relation: claim.Relation,
				SubmittedValue: append(json.RawMessage(nil), claim.SubmittedValue...),
				ValidFrom:      personFactPortableTimePointer(claim.ValidFrom),
				ValidUntil:     personFactPortableTimePointer(claim.ValidUntil),
				Origin:         claim.Origin, Confidence: claim.Confidence,
			},
			Normalized: copyPersonFactNormalized(claim.Normalized),
			Failure:    copyPersonFactFailure(claim.Failure),
		}
		if failure := preparedFailures[claim.ClaimKey]; failure != nil {
			item.Failure = copyPersonFactFailure(failure)
		}
		if item.Failure == nil && item.Normalized == nil {
			item.Failure = &personfacts.ValidationFailure{
				Action: personfacts.DecisionInvalid, Reason: personfacts.ReasonMalformedValue,
				Detail: "claim has no normalized value",
			}
		}
		for _, evidence := range evidenceByClaim[claim.ID] {
			if status, exists := statusByEvidence[evidence.ID]; exists {
				evidence.Supported = status.Supported
				evidence.LatestStatus = &status
			}
			item.Evidence = append(item.Evidence, evidence)
			item.Claim.Evidence = append(item.Claim.Evidence, evidence.Input)
		}
		resolvedAt, exists := chronologyByGeneration[claim.GenerationID]
		if !exists {
			return nil, nil, fmt.Errorf("load person fact claim generation %d: %w",
				claim.GenerationID, sql.ErrNoRows)
		}
		resolved = append(resolved, item)
		chronology[item.ClaimKey] = personFactClaimChronology{
			ResolvedAt: resolvedAt, GenerationID: claim.GenerationID,
		}
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].ClaimKey < resolved[j].ClaimKey })
	return resolved, chronology, nil
}

func loadPersonFactTargetClaimsTx(
	ctx context.Context, tx *loggedTx, personID int64, descriptor personfacts.TargetDescriptor,
) ([]personfacts.Claim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+personFactClaimColumns+`
		FROM person_fact_claims c
		WHERE c.person_id = ? AND c.target_kind = ? AND c.target_key = ?
		ORDER BY c.id DESC`, personID, descriptor.Kind, descriptor.Key)
	if err != nil {
		return nil, fmt.Errorf("load person fact target claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	claims := make([]personfacts.Claim, 0)
	for rows.Next() {
		claim, scanErr := scanPersonFactClaim(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact target claim: %w", scanErr)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact target claims: %w", err)
	}
	return claims, nil
}

func loadPersonFactTargetChronologyTx(
	ctx context.Context, tx *loggedTx, personID int64, descriptor personfacts.TargetDescriptor,
) (map[int64]time.Time, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT g.id, g.resolved_at
		FROM person_fact_generations g
		WHERE g.person_id = ? AND EXISTS (
			SELECT 1 FROM person_fact_claims c
			WHERE c.generation_id = g.id AND c.person_id = g.person_id
			  AND c.target_kind = ? AND c.target_key = ?)
		ORDER BY g.id`, personID, descriptor.Kind, descriptor.Key)
	if err != nil {
		return nil, fmt.Errorf("load person fact target generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	chronology := make(map[int64]time.Time)
	for rows.Next() {
		var generationID int64
		var resolvedAt personFactTimestamp
		if err := rows.Scan(&generationID, &resolvedAt); err != nil {
			return nil, fmt.Errorf("scan person fact target generation: %w", err)
		}
		chronology[generationID] = resolvedAt.Time
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact target generations: %w", err)
	}
	return chronology, nil
}

func loadPersonFactTargetEvidenceTx(
	ctx context.Context, tx *loggedTx, personID int64, descriptor personfacts.TargetDescriptor,
) (map[int64][]personfacts.Evidence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ce.claim_id, `+personFactEvidenceColumns+`
		FROM person_fact_claim_evidence ce
		JOIN person_fact_claims c ON c.id = ce.claim_id
		JOIN person_fact_evidence e ON e.id = ce.evidence_id AND e.person_id = c.person_id
		WHERE c.person_id = ? AND c.target_kind = ? AND c.target_key = ?
		ORDER BY ce.claim_id, e.id`, personID, descriptor.Kind, descriptor.Key)
	if err != nil {
		return nil, fmt.Errorf("load person fact target evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	evidenceByClaim := make(map[int64][]personfacts.Evidence)
	for rows.Next() {
		var claimID int64
		evidence, scanErr := scanPersonFactEvidenceWithPrefix(rows, &claimID)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact target evidence: %w", scanErr)
		}
		evidenceByClaim[claimID] = append(evidenceByClaim[claimID], evidence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact target evidence: %w", err)
	}
	return evidenceByClaim, nil
}

func loadPersonFactTargetEvidenceStatusesTx(
	ctx context.Context, tx *loggedTx, personID int64, descriptor personfacts.TargetDescriptor,
) (map[int64]personfacts.EvidenceStatusEvent, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT latest.id, latest.person_id, latest.generation_id, latest.evidence_id,
		       latest.evidence_key, latest.source_version, latest.supported,
		       latest.reason, latest.created_at
		FROM person_fact_evidence_status_events latest
		WHERE latest.person_id = ? AND EXISTS (
			SELECT 1 FROM person_fact_claim_evidence ce
			JOIN person_fact_claims c ON c.id = ce.claim_id
			WHERE ce.evidence_id = latest.evidence_id AND c.person_id = latest.person_id
			  AND c.target_kind = ? AND c.target_key = ?)
		  AND latest.id = (
			SELECT candidate.id
			FROM person_fact_evidence_status_events candidate
			JOIN person_fact_generations candidate_generation
			  ON candidate_generation.id = candidate.generation_id
			WHERE candidate.person_id = latest.person_id
			  AND candidate.evidence_key = latest.evidence_key
			  AND candidate.source_version = latest.source_version
			ORDER BY candidate_generation.resolved_at DESC, candidate.id DESC
			LIMIT 1)
		ORDER BY latest.id`, personID, descriptor.Kind, descriptor.Key)
	if err != nil {
		return nil, fmt.Errorf("load person fact target evidence statuses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	statuses := make(map[int64]personfacts.EvidenceStatusEvent)
	for rows.Next() {
		status, scanErr := scanPersonFactStatusEvent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact target evidence status: %w", scanErr)
		}
		statuses[status.EvidenceID] = status
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact target evidence statuses: %w", err)
	}
	return statuses, nil
}

func (s *Store) loadPersonFactGenerationResultTx(
	ctx context.Context, tx *loggedTx, personID int64, generationKey string,
) (*personfacts.GenerationResult, error) {
	ledger, err := s.loadPersonFactGenerationTx(ctx, tx, personID, generationKey)
	if err != nil {
		return nil, err
	}
	result := &personfacts.GenerationResult{
		GenerationID: ledger.Generation.ID, GenerationKey: ledger.Generation.GenerationKey,
		EvidenceStatusEvents: append([]personfacts.EvidenceStatusEvent(nil), ledger.EvidenceStatusEvents...),
		Resolutions:          append([]personfacts.ResolutionResult(nil), ledger.Resolutions...),
		Decisions:            make([]personfacts.Decision, 0),
		Projections:          make([]personfacts.ProjectionRef, 0),
	}
	projectionSet := make(map[personfacts.ProjectionRef]struct{})
	for _, resolution := range result.Resolutions {
		result.Decisions = append(result.Decisions, resolution.Decisions...)
		for _, projection := range resolution.Projections {
			projectionSet[projection] = struct{}{}
		}
	}
	sort.Slice(result.EvidenceStatusEvents, func(i, j int) bool {
		return result.EvidenceStatusEvents[i].ID < result.EvidenceStatusEvents[j].ID
	})
	sort.Slice(result.Resolutions, func(i, j int) bool {
		left, right := result.Resolutions[i], result.Resolutions[j]
		if left.Target.Kind != right.Target.Kind {
			return left.Target.Kind < right.Target.Kind
		}
		if left.Target.Key != right.Target.Key {
			return left.Target.Key < right.Target.Key
		}
		if left.Target.Revision != right.Target.Revision {
			return left.Target.Revision < right.Target.Revision
		}
		return left.ID < right.ID
	})
	sort.Slice(result.Decisions, func(i, j int) bool {
		left, right := result.Decisions[i], result.Decisions[j]
		if left.ResolutionID != right.ResolutionID {
			return left.ResolutionID < right.ResolutionID
		}
		if left.ClaimKey != right.ClaimKey {
			return left.ClaimKey < right.ClaimKey
		}
		return left.Action < right.Action
	})
	for projection := range projectionSet {
		result.Projections = append(result.Projections, projection)
	}
	sortPersonFactProjectionRefs(result.Projections)
	if result.EvidenceStatusEvents == nil {
		result.EvidenceStatusEvents = []personfacts.EvidenceStatusEvent{}
	}
	if result.Resolutions == nil {
		result.Resolutions = []personfacts.ResolutionResult{}
	}
	return result, nil
}

func (s *Store) personFactProjectorTx(
	ctx context.Context, tx *loggedTx, target personfacts.TargetDescriptor,
) (personFactTargetProjector, error) {
	return s.personFactProjectorWithOrganizationLocksTx(ctx, tx, target, nil)
}

func (s *Store) personFactProjectorWithOrganizationLocksTx(
	ctx context.Context, tx *loggedTx, target personfacts.TargetDescriptor,
	organizationLocks *personFactOrganizationLockSet,
) (personFactTargetProjector, error) {
	return s.personFactProjector(ctx, tx, target, true, organizationLocks)
}

func (s *Store) personFactProjectorSnapshotTx(
	ctx context.Context, tx *loggedTx, target personfacts.TargetDescriptor,
) (personFactTargetProjector, error) {
	return s.personFactProjector(ctx, tx, target, false, nil)
}

func (s *Store) personFactProjector(
	ctx context.Context, tx *loggedTx, target personfacts.TargetDescriptor, lock bool,
	organizationLocks *personFactOrganizationLockSet,
) (personFactTargetProjector, error) {
	switch target.Kind {
	case personfacts.TargetAttribute:
		definition, err := s.getAttributeDefinitionByUniversalID(ctx, tx, target.Key, lock)
		if err != nil {
			return nil, err
		}
		return &personFactAttributeProjector{store: s, tx: tx, definition: *definition}, nil
	case personfacts.TargetEmployment:
		return &personFactEmploymentProjector{
			store: s, tx: tx, lock: lock, organizationLocks: organizationLocks,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported person fact target kind %q", target.Kind)
	}
}

func (p *personFactAttributeProjector) loadCurrent(
	ctx context.Context, personID int64, target personfacts.TargetDescriptor,
) ([]personfacts.CurrentProjection, error) {
	values, err := p.store.listPersonAttributeValuesContext(ctx, p.tx, personID,
		PersonAttributeQuery{DefinitionSlug: p.definition.Slug})
	if err != nil {
		return nil, err
	}
	current := make([]personfacts.CurrentProjection, 0, len(values))
	for _, value := range values {
		normalized, normalizeErr := normalizedPersonFactAttributeValue(target, value.Value)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		current = append(current, personfacts.CurrentProjection{
			Ref:        personfacts.ProjectionRef{Kind: "person_attribute", RowID: value.ID},
			Normalized: *normalized, ActiveFrom: value.ActiveFrom,
			ActiveUntil: value.ActiveUntil, TransactionTime: value.CreatedAt,
			Declared: value.Source.IsDeclared(),
		})
	}
	return current, nil
}

func (p *personFactAttributeProjector) projectionContext(
	context.Context, int64, []personfacts.ResolvedClaim,
) (string, error) {
	return "", nil
}

func (p *personFactEmploymentProjector) loadCurrent(
	ctx context.Context, personID int64, target personfacts.TargetDescriptor,
) ([]personfacts.CurrentProjection, error) {
	if p.lock {
		if p.organizationLocks == nil {
			refs, err := p.store.loadPersonFactEmploymentOrganizationReferencesTx(
				ctx, p.tx, personID, map[string]struct{}{target.Key: {}})
			if err != nil {
				return nil, err
			}
			if err := p.store.lockPersonFactOrganizationTableForReferencesTx(
				ctx, p.tx, refs); err != nil {
				return nil, err
			}
		}
		if err := p.store.lockEmploymentPeopleTx(ctx, p.tx, personID); err != nil {
			return nil, err
		}
	}
	employments, err := p.store.listAllEmploymentsContext(
		ctx, p.tx, EmploymentFilter{PersonID: personID})
	if err != nil {
		return nil, err
	}
	ownedHistorical, err := loadOwnedPersonFactEmploymentProjectionIDsTx(ctx, p.tx, personID)
	if err != nil {
		return nil, err
	}
	current := make([]personfacts.CurrentProjection, 0, len(employments))
	for _, employment := range employments {
		if _, owned := ownedHistorical[employment.ID]; !employment.IsCurrent && !owned {
			continue
		}
		organization, loadErr := getOrganizationTx(ctx, p.tx, employment.OrganizationID)
		if loadErr != nil {
			return nil, loadErr
		}
		normalized, normalizeErr := normalizedPersonFactEmploymentValue(target, employment, *organization)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		current = append(current, personfacts.CurrentProjection{
			Ref:        personfacts.ProjectionRef{Kind: personFactProjectionKindEmployment, RowID: employment.ID},
			Normalized: *normalized, ActiveFrom: personFactEmploymentActiveFrom(employment),
			TransactionTime: employment.UpdatedAt.UTC(), Declared: employment.Source.IsDeclared(),
		})
	}
	return current, nil
}

func loadOwnedPersonFactEmploymentProjectionIDsTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT e.id
		FROM employments e
		JOIN person_fact_decisions d
		  ON d.person_id = e.person_id
		 AND d.projection_kind = 'employment'
		 AND d.projection_row_id = e.id
		WHERE e.person_id = ? AND e.source_ref = ? || d.decision_key`,
		personID, personFactDecisionSourceRefPrefix)
	if err != nil {
		return nil, fmt.Errorf("load owned employment projections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owned := make(map[int64]struct{})
	for rows.Next() {
		var rowID int64
		if err := rows.Scan(&rowID); err != nil {
			return nil, fmt.Errorf("scan owned employment projection: %w", err)
		}
		owned[rowID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owned employment projections: %w", err)
	}
	return owned, nil
}

func (p *personFactEmploymentProjector) projectionContext(
	ctx context.Context, personID int64, claims []personfacts.ResolvedClaim,
) (string, error) {
	type preparedClaim struct {
		index int
		value personfacts.EmploymentValue
		ref   personfacts.OrganizationReference
		keys  []personFactOrganizationLookupKey
	}
	preparedClaims := make([]preparedClaim, 0, len(claims))
	claimKeys := make([]string, 0, len(claims))
	for index := range claims {
		if claims[index].Failure == nil && claims[index].Normalized != nil {
			claimKeys = append(claimKeys, claims[index].ClaimKey)
		}
	}
	organizationBindings, err := loadPersonFactEmploymentOrganizationBindingsTx(
		ctx, p.tx, personID, claimKeys)
	if err != nil {
		return "", err
	}
	for index := range claims {
		if claims[index].Failure != nil || claims[index].Normalized == nil {
			continue
		}
		var value personfacts.EmploymentValue
		if err := json.Unmarshal(claims[index].Normalized.JSON, &value); err != nil {
			return "", fmt.Errorf("decode normalized employment claim %q: %w", claims[index].ClaimKey, err)
		}
		if organizationID, bound := organizationBindings[claims[index].ClaimKey]; bound {
			value.Organization.ID = &organizationID
		}
		ref, keys, err := normalizePersonFactOrganizationReference(value.Organization)
		if err != nil {
			return "", err
		}
		value.Organization = ref
		preparedClaims = append(preparedClaims, preparedClaim{
			index: index, value: value, ref: ref, keys: keys,
		})
	}
	lockSet := p.organizationLocks
	if lockSet == nil {
		refs := make([]personfacts.OrganizationReference, len(preparedClaims))
		for index := range preparedClaims {
			refs[index] = preparedClaims[index].ref
		}
		var err error
		lockSet, err = p.store.lockPersonFactOrganizationReferencesTx(ctx, p.tx, refs)
		if err != nil {
			return "", err
		}
	}
	p.organizationLocks = lockSet

	type fingerprintEntry struct {
		ClaimKey    string                       `json:"claim_key"`
		Fingerprint string                       `json:"match_fingerprint"`
		Status      OrganizationResolutionStatus `json:"status"`
		Candidates  []int64                      `json:"candidate_ids"`
	}
	entries := make([]fingerprintEntry, 0, len(preparedClaims))
	for _, prepared := range preparedClaims {
		match, err := p.store.prepareLockedPersonFactOrganizationReferenceTx(
			ctx, p.tx, prepared.ref, prepared.keys, lockSet, false)
		if err != nil {
			failure := personFactOrganizationReferenceFailure(err)
			if failure == nil {
				return "", err
			}
			claim := &claims[prepared.index]
			claim.Normalized = nil
			claim.Failure = failure
			continue
		}
		claim := &claims[prepared.index]
		if match.Status == OrganizationAmbiguous {
			claim.Failure = &personfacts.ValidationFailure{
				Action: personfacts.DecisionAmbiguousRetained,
				Reason: personfacts.ReasonOrganizationAmbiguous,
				Detail: "organization reference matches multiple active companies",
			}
		} else if match.Status == OrganizationReused && match.Organization != nil {
			prepared.value.Organization = canonicalPersonFactOrganizationReference(*match.Organization)
			normalized, failure, normalizeErr := personfacts.NormalizeClaimValue(
				claim.Claim.Target, mustMarshalPersonFactEmployment(prepared.value))
			if normalizeErr != nil {
				return "", normalizeErr
			}
			if failure != nil {
				return "", fmt.Errorf("normalize resolved employment organization: %s", failure.Detail)
			}
			claim.Normalized = normalized
		}
		entries = append(entries, fingerprintEntry{
			ClaimKey: claim.ClaimKey, Fingerprint: match.Fingerprint, Status: match.Status,
			Candidates: append([]int64(nil), match.CandidateIDs...),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ClaimKey < entries[j].ClaimKey })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("encode employment projection context: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func loadPersonFactEmploymentOrganizationBindingsTx(
	ctx context.Context, tx *loggedTx, personID int64, claimKeys []string,
) (map[string]int64, error) {
	bindings := make(map[string]int64)
	if len(claimKeys) == 0 {
		return bindings, nil
	}
	args := make([]any, 1, len(claimKeys)+1)
	args[0] = personID
	for _, claimKey := range claimKeys {
		args = append(args, claimKey)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.claim_key, d.resolved_organization_id
		FROM person_fact_decisions d
		JOIN person_fact_claims c ON c.id = d.claim_id
		WHERE d.person_id = ? AND d.action = 'applied'
		  AND d.resolved_organization_id IS NOT NULL
		  AND c.claim_key IN (`+placeholders(len(claimKeys))+`)
		ORDER BY c.claim_key, d.id DESC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("load employment organization bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var claimKey string
		var organizationID int64
		if err := rows.Scan(&claimKey, &organizationID); err != nil {
			return nil, fmt.Errorf("scan employment organization binding: %w", err)
		}
		if _, exists := bindings[claimKey]; !exists {
			bindings[claimKey] = organizationID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate employment organization bindings: %w", err)
	}
	return bindings, nil
}

func (s *Store) reconcilePersonFactEmploymentClaimsTx(
	ctx context.Context, tx *loggedTx, personID int64, claims []personfacts.ResolvedClaim,
	chronology map[string]personFactClaimChronology,
) ([]personfacts.ResolvedClaim, error) {
	// Employment corrections and retirements form a timeline per canonical
	// organization/title identity. Only the newest generation may project again.
	projected, err := loadProjectedPersonFactEmploymentClaimsTx(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	identities := make([]string, len(claims))
	endDated := make([]bool, len(claims))
	latest := make(map[string]personFactClaimChronology)
	type projectedEpisode struct {
		chronology  personFactClaimChronology
		relation    personfacts.ClaimRelation
		fingerprint string
	}
	projectedByIdentity := make(map[string]projectedEpisode)
	for index := range claims {
		if claims[index].Failure != nil || claims[index].Normalized == nil {
			continue
		}
		identity, historical, identityErr := personFactEmploymentStableIdentity(claims[index])
		if identityErr != nil {
			return nil, identityErr
		}
		claimChronology, exists := chronology[claims[index].ClaimKey]
		if !exists {
			return nil, fmt.Errorf("employment claim %q has no ledger chronology", claims[index].ClaimKey)
		}
		identities[index] = identity
		endDated[index] = historical
		if current, exists := latest[identity]; !exists || personFactChronologyBefore(current, claimChronology) {
			latest[identity] = claimChronology
		}
		if rowID, exists := projected[claims[index].ClaimKey]; exists {
			matches, matchErr := s.personFactEmploymentProjectionMatchesClaimTx(
				ctx, tx, personID, rowID, claims[index])
			if matchErr != nil {
				return nil, matchErr
			}
			if !matches {
				continue
			}
			projection := projectedEpisode{
				chronology: claimChronology, relation: claims[index].Claim.Relation,
				fingerprint: claims[index].Normalized.Fingerprint,
			}
			if current, exists := projectedByIdentity[identity]; !exists || personFactChronologyBefore(current.chronology, claimChronology) {
				projectedByIdentity[identity] = projection
			}
		}
	}

	reconciled := make([]personfacts.ResolvedClaim, 0, len(claims))
	for index, claim := range claims {
		identity := identities[index]
		if identity == "" {
			reconciled = append(reconciled, claim)
			continue
		}
		claimChronology := chronology[claim.ClaimKey]
		if !samePersonFactChronology(claimChronology, latest[identity]) {
			continue
		}
		if claim.Claim.Relation == personfacts.RelationSupport && endDated[index] {
			projection, alreadyProjected := projectedByIdentity[identity]
			if alreadyProjected && projection.relation == personfacts.RelationSupport &&
				projection.fingerprint == claim.Normalized.Fingerprint {
				continue
			}
		}
		reconciled = append(reconciled, claim)
	}
	return reconciled, nil
}

func reconcilePersonFactEmploymentCompetition(
	resolution *personfacts.Resolution, claims []personfacts.ResolvedClaim,
) error {
	type direction struct {
		indexes        []int
		representative int
	}
	claimByKey := make(map[string]personfacts.ResolvedClaim, len(claims))
	for _, claim := range claims {
		claimByKey[claim.ClaimKey] = claim
	}
	byIdentityFingerprint := make(map[string]map[string][]int)
	for index := range resolution.Decisions {
		decision := resolution.Decisions[index]
		claim, exists := claimByKey[decision.ClaimKey]
		if !exists {
			return fmt.Errorf("employment decision references unknown claim %q", decision.ClaimKey)
		}
		if claim.Failure != nil || claim.Normalized == nil ||
			claim.Claim.Relation != personfacts.RelationSupport {
			continue
		}
		// These are the complete outcomes for an eligible support-only
		// fingerprint direction after resolveMulti. Other outcomes are either
		// ineligible claims or support directions defeated by an explicit
		// contradiction/supersession and must not be reactivated here.
		eligible := decision.Action == personfacts.DecisionApplied
		if decision.Action == personfacts.DecisionSuperseded {
			eligible = decision.Reason == personfacts.ReasonAppliedProjection
		}
		if decision.Action == personfacts.DecisionRetained {
			eligible = decision.Reason == personfacts.ReasonBelowThreshold
		}
		if !eligible {
			continue
		}
		identity, historical, err := personFactEmploymentStableIdentity(claim)
		if err != nil {
			return err
		}
		if historical {
			continue
		}
		if byIdentityFingerprint[identity] == nil {
			byIdentityFingerprint[identity] = make(map[string][]int)
		}
		fingerprint := claim.Normalized.Fingerprint
		byIdentityFingerprint[identity][fingerprint] = append(
			byIdentityFingerprint[identity][fingerprint], index)
	}
	byIdentity := make(map[string][]direction, len(byIdentityFingerprint))
	for identity, byFingerprint := range byIdentityFingerprint {
		for _, indexes := range byFingerprint {
			sort.Slice(indexes, func(i, j int) bool {
				left := resolution.Decisions[indexes[i]]
				right := resolution.Decisions[indexes[j]]
				if left.Score.Total != right.Score.Total {
					return left.Score.Total > right.Score.Total
				}
				return left.ClaimKey < right.ClaimKey
			})
			representative := indexes[0]
			for _, index := range indexes {
				if resolution.Decisions[index].Action == personfacts.DecisionApplied {
					representative = index
					break
				}
			}
			byIdentity[identity] = append(byIdentity[identity], direction{
				indexes: indexes, representative: representative,
			})
		}
	}

	notApplied := make(map[string]struct{})
	setDecision := func(index int, action personfacts.DecisionAction, reason personfacts.DecisionReason, competing string) error {
		decision := &resolution.Decisions[index]
		decision.Action = action
		decision.Reason = reason
		decision.CompetingClaimKey = competing
		decision.Projection = nil
		key, err := personfacts.DecisionKey(
			resolution.InputFingerprint, decision.ClaimKey, decision.Action)
		if err != nil {
			return err
		}
		decision.DecisionKey = key
		if action == personfacts.DecisionApplied {
			delete(notApplied, decision.ClaimKey)
		} else {
			notApplied[decision.ClaimKey] = struct{}{}
		}
		return nil
	}
	setDirection := func(item direction, action personfacts.DecisionAction, reason personfacts.DecisionReason, competing string) error {
		for _, index := range item.indexes {
			if err := setDecision(index, action, reason, competing); err != nil {
				return err
			}
		}
		return nil
	}
	setWinningDirection := func(item direction) error {
		representative := resolution.Decisions[item.representative].ClaimKey
		for _, index := range item.indexes {
			action := personfacts.DecisionSuperseded
			competing := representative
			if index == item.representative {
				action = personfacts.DecisionApplied
				competing = ""
			}
			if err := setDecision(index, action, personfacts.ReasonAppliedProjection, competing); err != nil {
				return err
			}
		}
		return nil
	}
	policy := personfacts.DefaultPolicyV1()
	for _, directions := range byIdentity {
		if len(directions) < 2 {
			continue
		}
		sort.Slice(directions, func(i, j int) bool {
			left := resolution.Decisions[directions[i].representative]
			right := resolution.Decisions[directions[j].representative]
			if left.Score.Total != right.Score.Total {
				return left.Score.Total > right.Score.Total
			}
			return left.ClaimKey < right.ClaimKey
		})
		winner := resolution.Decisions[directions[0].representative].ClaimKey
		if resolution.Decisions[directions[0].representative].Score.Total < policy.ApplyThreshold {
			for _, item := range directions {
				if err := setDirection(item, personfacts.DecisionRetained,
					personfacts.ReasonBelowThreshold, ""); err != nil {
					return err
				}
			}
			continue
		}
		if resolution.Decisions[directions[0].representative].Score.Total ==
			resolution.Decisions[directions[1].representative].Score.Total {
			tied := 2
			for tied < len(directions) &&
				resolution.Decisions[directions[tied].representative].Score.Total ==
					resolution.Decisions[directions[0].representative].Score.Total {
				tied++
			}
			for tiedIndex, item := range directions[:tied] {
				competing := resolution.Decisions[directions[(tiedIndex+1)%tied].representative].ClaimKey
				if err := setDirection(item, personfacts.DecisionAmbiguousRetained,
					personfacts.ReasonCompetingTie, competing); err != nil {
					return err
				}
			}
			for _, item := range directions[tied:] {
				if err := setDirection(item, personfacts.DecisionConflictRejected,
					personfacts.ReasonInsufficientMargin, winner); err != nil {
					return err
				}
			}
			continue
		}
		if resolution.Decisions[directions[0].representative].Score.Total-
			resolution.Decisions[directions[1].representative].Score.Total <
			policy.ReplacementMargin {
			if err := setDirection(directions[0], personfacts.DecisionRetained,
				personfacts.ReasonInsufficientMargin,
				resolution.Decisions[directions[1].representative].ClaimKey); err != nil {
				return err
			}
			for _, item := range directions[1:] {
				if err := setDirection(item, personfacts.DecisionConflictRejected,
					personfacts.ReasonInsufficientMargin, winner); err != nil {
					return err
				}
			}
			continue
		}
		if err := setWinningDirection(directions[0]); err != nil {
			return err
		}
		for _, item := range directions[1:] {
			if err := setDirection(item, personfacts.DecisionSuperseded,
				personfacts.ReasonAppliedProjection, winner); err != nil {
				return err
			}
		}
	}
	if len(notApplied) == 0 {
		return nil
	}
	plans := resolution.Projections[:0]
	for _, plan := range resolution.Projections {
		if _, exists := notApplied[plan.ClaimKey]; !exists {
			plans = append(plans, plan)
		}
	}
	resolution.Projections = plans
	return nil
}

func (s *Store) personFactEmploymentProjectionMatchesClaimTx(
	ctx context.Context, tx *loggedTx, personID, rowID int64, claim personfacts.ResolvedClaim,
) (bool, error) {
	employment, err := getEmploymentForUpdateTx(ctx, tx, s.dialect, rowID)
	if errors.Is(err, ErrEmploymentNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if employment.PersonID != personID {
		return false, nil
	}
	organization, err := getOrganizationForUpdateTx(ctx, tx, s.dialect, employment.OrganizationID)
	if err != nil {
		return false, err
	}
	actual, err := normalizedPersonFactEmploymentValue(claim.Claim.Target, *employment, *organization)
	if err != nil {
		return false, err
	}
	return claim.Normalized != nil && actual.Fingerprint == claim.Normalized.Fingerprint, nil
}

func loadProjectedPersonFactEmploymentClaimsTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.claim_key, d.projection_row_id
		FROM person_fact_decisions d
		JOIN person_fact_claims c ON c.id = d.claim_id
		WHERE d.person_id = ? AND d.action = 'applied'
		  AND d.projection_kind = 'employment' AND d.projection_row_id IS NOT NULL
		ORDER BY c.claim_key, d.id DESC`, personID)
	if err != nil {
		return nil, fmt.Errorf("load projected employment claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projected := make(map[string]int64)
	for rows.Next() {
		var claimKey string
		var rowID int64
		if scanErr := rows.Scan(&claimKey, &rowID); scanErr != nil {
			return nil, fmt.Errorf("scan projected employment claim: %w", scanErr)
		}
		if _, exists := projected[claimKey]; !exists {
			projected[claimKey] = rowID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projected employment claims: %w", err)
	}
	return projected, nil
}

func personFactEmploymentStableIdentity(
	claim personfacts.ResolvedClaim,
) (string, bool, error) {
	if claim.Normalized == nil {
		return "", false, fmt.Errorf("employment claim %q has no normalized value", claim.ClaimKey)
	}
	return personFactEmploymentNormalizedStableIdentity(claim.ClaimKey, *claim.Normalized)
}

func personFactEmploymentNormalizedStableIdentity(
	claimKey string, normalized personfacts.NormalizedValue,
) (string, bool, error) {
	var value personfacts.EmploymentValue
	if err := json.Unmarshal(normalized.JSON, &value); err != nil {
		return "", false, fmt.Errorf("decode employment claim %q for chronology: %w", claimKey, err)
	}
	_, keys, err := normalizePersonFactOrganizationReference(value.Organization)
	if err != nil {
		return "", false, fmt.Errorf("normalize employment claim %q organization for chronology: %w",
			claimKey, err)
	}
	organization, err := json.Marshal(keys)
	if err != nil {
		return "", false, fmt.Errorf("encode employment claim %q organization identity: %w",
			claimKey, err)
	}
	title := value.Title
	return string(organization) + "\x00" + NormalizeEmploymentTitle(&title), value.EndDate != nil, nil
}

func personFactChronologyBefore(left, right personFactClaimChronology) bool {
	if !left.ResolvedAt.Equal(right.ResolvedAt) {
		return left.ResolvedAt.Before(right.ResolvedAt)
	}
	return left.GenerationID < right.GenerationID
}

func samePersonFactChronology(left, right personFactClaimChronology) bool {
	return left.ResolvedAt.Equal(right.ResolvedAt) && left.GenerationID == right.GenerationID
}

func (p *personFactEmploymentProjector) retireExpired(
	ctx context.Context, personID int64, resolvedAt time.Time,
	current []personfacts.CurrentProjection, claims []personfacts.ResolvedClaim,
	decisions []personfacts.Decision, planned map[personfacts.ProjectionRef]struct{},
) ([]personFactRetiredProjection, bool, error) {
	type expiry struct {
		claim       personfacts.ResolvedClaim
		decisionKey string
		validUntil  time.Time
	}
	claimByKey := make(map[string]personfacts.ResolvedClaim, len(claims))
	for _, claim := range claims {
		claimByKey[claim.ClaimKey] = claim
	}
	live := make(map[string]struct{})
	expired := make(map[string]expiry)
	for _, decision := range decisions {
		claim, exists := claimByKey[decision.ClaimKey]
		if !exists || claim.Normalized == nil ||
			claim.Claim.Relation != personfacts.RelationSupport {
			continue
		}
		identity, _, err := personFactEmploymentStableIdentity(claim)
		if err != nil {
			return nil, false, err
		}
		if decision.Reason == personfacts.ReasonEvidenceUnsupported {
			live[identity] = struct{}{}
			continue
		}
		if decision.Reason != personfacts.ReasonOutsideValidity {
			if decision.Action != personfacts.DecisionInvalid &&
				decision.Action != personfacts.DecisionIdentityRejected &&
				decision.Action != personfacts.DecisionPolicyRejected {
				live[identity] = struct{}{}
			}
			continue
		}
		if claim.Claim.ValidUntil == nil || resolvedAt.Before(*claim.Claim.ValidUntil) {
			continue
		}
		candidate := expiry{
			claim: claim, decisionKey: decision.DecisionKey,
			validUntil: claim.Claim.ValidUntil.UTC(),
		}
		previous, exists := expired[identity]
		if !exists || previous.validUntil.Before(candidate.validUntil) ||
			(previous.validUntil.Equal(candidate.validUntil) &&
				candidate.claim.ClaimKey < previous.claim.ClaimKey) {
			expired[identity] = candidate
		}
	}

	retired := make([]personFactRetiredProjection, 0)
	for _, item := range current {
		if item.Declared {
			continue
		}
		if _, exists := planned[item.Ref]; exists {
			continue
		}
		identity, _, err := personFactEmploymentNormalizedStableIdentity(
			fmt.Sprintf("projection:%d", item.Ref.RowID), item.Normalized)
		if err != nil {
			return nil, false, err
		}
		if _, exists := live[identity]; exists {
			continue
		}
		candidate, exists := expired[identity]
		if !exists {
			continue
		}
		owned, err := ownsPersonFactProjectionTx(ctx, p.tx, personID, item.Ref)
		if err != nil {
			return nil, false, err
		}
		if !owned {
			continue
		}
		employment, err := getEmploymentForUpdateTx(
			ctx, p.tx, p.store.dialect, item.Ref.RowID)
		if err != nil {
			return nil, false, err
		}
		if employment.PersonID != personID {
			return nil, false, ErrEmploymentRevisionConflict
		}
		if !employment.IsCurrent {
			continue
		}
		endDate := PartialDate{
			Year: new(candidate.validUntil.Year()), Month: new(int(candidate.validUntil.Month())),
			Day: new(candidate.validUntil.Day()),
		}
		if employment.StartDate != nil &&
			CompareAtSharedPrecision(endDate, *employment.StartDate) < 0 {
			endDate = *employment.StartDate
		}
		source, err := personFactEmploymentProvenance(candidate.claim.Claim.Origin)
		if err != nil {
			return nil, false, err
		}
		ended, err := p.store.endEmploymentProjectionTx(
			ctx, p.tx, employment, endDate, source,
			personFactDecisionSourceRefPrefix+candidate.decisionKey,
			float64(candidate.claim.Claim.Confidence.ReportedScore)/1000)
		if err != nil {
			return nil, false, err
		}
		retired = append(retired, personFactRetiredProjection{
			Ref:      personfacts.ProjectionRef{Kind: personFactProjectionKindEmployment, RowID: ended.ID},
			ClaimKey: candidate.claim.ClaimKey,
		})
	}
	return retired, len(retired) > 0, nil
}

func (p *personFactEmploymentProjector) project(
	ctx context.Context, plan personfacts.ProjectionPlan, resolved personfacts.ResolvedClaim,
	decisionKey string, transactionTime time.Time,
) (*personfacts.ProjectionRef, bool, error) {
	if p.consumed == nil {
		p.consumed = make(map[int64]struct{})
	}
	ref, err := p.store.projectEmploymentFactTx(
		ctx, p.tx, plan, resolved, p.organizationLocks, p.consumed, decisionKey, transactionTime)
	if ref != nil {
		p.consumed[ref.RowID] = struct{}{}
	}
	return ref, ref != nil, err
}

func (s *Store) projectEmploymentFactTx(
	ctx context.Context, tx *loggedTx, plan personfacts.ProjectionPlan,
	resolved personfacts.ResolvedClaim, organizationLocks *personFactOrganizationLockSet,
	consumed map[int64]struct{}, decisionKey string, transactionTime time.Time,
) (*personfacts.ProjectionRef, error) {
	if transactionTime.IsZero() || decisionKey == "" {
		return nil, errors.New("employment fact projection metadata is required")
	}
	if plan.Target.Kind != personfacts.TargetEmployment {
		return nil, fmt.Errorf("employment projector cannot project target %q", plan.Target.Kind)
	}
	claim, err := scanPersonFactClaim(tx.QueryRowContext(ctx,
		`SELECT `+personFactClaimColumns+` FROM person_fact_claims c WHERE c.claim_key = ?`,
		plan.ClaimKey))
	if err != nil {
		return nil, fmt.Errorf("load employment projection claim %q: %w", plan.ClaimKey, err)
	}
	if err := s.lockEmploymentPeopleTx(ctx, tx, claim.PersonID); err != nil {
		return nil, err
	}
	source, err := personFactEmploymentProvenance(claim.Origin)
	if err != nil {
		return nil, err
	}
	sourceRef := personFactDecisionSourceRefPrefix + decisionKey
	confidence := float64(claim.Confidence.ReportedScore) / 1000

	switch plan.Operation {
	case personfacts.ProjectionRetire:
		if plan.CurrentRef == nil || plan.CurrentRef.Kind != "employment" {
			return nil, errors.New("employment retirement requires an exact current employment reference")
		}
		current, err := getEmploymentForUpdateTx(ctx, tx, s.dialect, plan.CurrentRef.RowID)
		if err != nil {
			return nil, err
		}
		if current.PersonID != claim.PersonID {
			return nil, ErrEmploymentRevisionConflict
		}
		if !current.IsCurrent {
			owned, ownershipErr := ownsPersonFactProjectionTx(
				ctx, tx, claim.PersonID, *plan.CurrentRef)
			if ownershipErr != nil {
				return nil, ownershipErr
			}
			if !owned {
				return nil, ErrEmploymentRevisionConflict
			}
			var deletedID int64
			deleteErr := tx.QueryRowContext(ctx,
				`DELETE FROM employments WHERE id = ? AND revision = ? RETURNING id`,
				current.ID, current.Revision).Scan(&deletedID)
			if errors.Is(deleteErr, sql.ErrNoRows) {
				return nil, ErrEmploymentRevisionConflict
			}
			if deleteErr != nil {
				return nil, fmt.Errorf("retract historical employment projection: %w", deleteErr)
			}
			return &personfacts.ProjectionRef{
				Kind: personFactProjectionKindEmployment, RowID: deletedID,
			}, nil
		}
		endDate := PartialDate{
			Year: new(plan.ActiveFrom.Year()), Month: new(int(plan.ActiveFrom.Month())),
			Day: new(plan.ActiveFrom.Day()),
		}
		if current.StartDate != nil && CompareAtSharedPrecision(endDate, *current.StartDate) < 0 {
			endDate = *current.StartDate
		}
		ended, err := s.endEmploymentProjectionTx(
			ctx, tx, current, endDate, source, sourceRef, confidence)
		if err != nil {
			return nil, err
		}
		return &personfacts.ProjectionRef{Kind: personFactProjectionKindEmployment, RowID: ended.ID}, nil

	case personfacts.ProjectionSet:
		if resolved.ClaimKey != plan.ClaimKey {
			return nil, errors.New("employment projection resolved claim does not match its plan")
		}
		if resolved.Normalized == nil {
			return nil, errors.New("employment projection claim has no normalized value")
		}
		var value personfacts.EmploymentValue
		if err := json.Unmarshal(resolved.Normalized.JSON, &value); err != nil {
			return nil, fmt.Errorf("decode employment projection value: %w", err)
		}
		ref, keys, err := normalizePersonFactOrganizationReference(value.Organization)
		if err != nil {
			return nil, err
		}
		if organizationLocks == nil {
			return nil, errors.New("employment projection organization lock scope is required")
		}
		prepared, err := s.prepareLockedPersonFactOrganizationReferenceTx(
			ctx, tx, ref, keys, organizationLocks, true)
		if err != nil {
			return nil, err
		}
		organization, status, err := s.materializeLockedPersonFactOrganizationTx(
			ctx, tx, ref, prepared, organizationLocks)
		if err != nil {
			return nil, err
		}
		if status == OrganizationAmbiguous || organization == nil {
			return nil, errors.New("accepted employment projection has an ambiguous organization")
		}
		input := personFactEmploymentInput(
			claim.PersonID, organization.ID, value, source, sourceRef, confidence)
		if value.EndDate == nil {
			current, currentErr := scanEmployment(tx.QueryRowContext(ctx, fmt.Sprintf(`
				SELECT %s FROM employments
				WHERE person_id = ? AND organization_id = ? AND title_normalized = ?
				  AND %s%s
			`, employmentColumns, s.dialect.BoolTrueExpr("is_current"), s.dialect.SelectForUpdate()),
				claim.PersonID, organization.ID, NormalizeEmploymentTitle(input.Title)))
			if currentErr == nil {
				if _, alreadyConsumed := consumed[current.ID]; !alreadyConsumed {
					corrected, reviseErr := s.reviseEmploymentTx(
						ctx, tx, current.ID, current.Revision, input)
					if reviseErr != nil {
						return nil, reviseErr
					}
					return &personfacts.ProjectionRef{
						Kind: personFactProjectionKindEmployment, RowID: corrected.ID,
					}, nil
				}
			} else if !errors.Is(currentErr, sql.ErrNoRows) {
				return nil, fmt.Errorf("find correctable employment: %w", currentErr)
			}
		}
		projectedID, projectionErr := s.loadMatchingPersonFactEmploymentProjectionTx(
			ctx, tx, claim, input, consumed)
		if projectionErr == nil && value.EndDate != nil {
			historical, loadErr := getEmploymentForUpdateTx(ctx, tx, s.dialect, projectedID)
			if loadErr != nil {
				return nil, loadErr
			}
			corrected, reviseErr := s.reviseEmploymentTx(
				ctx, tx, historical.ID, historical.Revision, input)
			if reviseErr != nil {
				return nil, reviseErr
			}
			return &personfacts.ProjectionRef{Kind: personFactProjectionKindEmployment, RowID: corrected.ID}, nil
		}
		if projectionErr != nil && !errors.Is(projectionErr, sql.ErrNoRows) {
			return nil, projectionErr
		}
		inserted, err := s.addEmploymentTx(ctx, tx, input)
		if err != nil {
			return nil, err
		}
		return &personfacts.ProjectionRef{Kind: personFactProjectionKindEmployment, RowID: inserted.ID}, nil
	default:
		return nil, fmt.Errorf("unsupported employment projection operation %q", plan.Operation)
	}
}

func (s *Store) loadMatchingPersonFactEmploymentProjectionTx(
	ctx context.Context, tx *loggedTx, claim personfacts.Claim, input EmploymentInput,
	consumed map[int64]struct{},
) (int64, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s FROM employments
		WHERE person_id = ? AND organization_id = ? AND title_normalized = ?
		  AND NOT (%s) AND id IN (
			SELECT d.projection_row_id
			FROM person_fact_decisions d
			JOIN person_fact_claims c ON c.id = d.claim_id
			WHERE d.person_id = ? AND d.action = 'applied'
			  AND d.projection_kind = 'employment' AND d.projection_row_id IS NOT NULL
			  AND c.relation = 'support' AND c.origin = ?)
		ORDER BY id`, employmentColumns, s.dialect.BoolTrueExpr("is_current")),
		claim.PersonID, input.OrganizationID, NormalizeEmploymentTitle(input.Title),
		claim.PersonID, claim.Origin)
	if err != nil {
		return 0, fmt.Errorf("load employment projection candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type episodeCandidate struct {
		rowID int64
		score int
	}
	candidates := make([]episodeCandidate, 0)
	for rows.Next() {
		employment, scanErr := scanEmployment(rows)
		if scanErr != nil {
			return 0, fmt.Errorf("scan employment projection candidate: %w", scanErr)
		}
		if _, exists := consumed[employment.ID]; exists {
			continue
		}
		score := 0
		if samePersonFactEmploymentDate(employment.StartDate, input.StartDate) {
			score += 2
		}
		if samePersonFactEmploymentDate(employment.EndDate, input.EndDate) {
			score++
		}
		if personFactEmploymentDateRangesOverlap(employment, input) {
			score++
		}
		candidates = append(candidates, episodeCandidate{rowID: employment.ID, score: score})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate employment projection candidates: %w", err)
	}
	if len(candidates) == 1 && candidates[0].score > 0 {
		return candidates[0].rowID, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].rowID < candidates[j].rowID
	})
	if len(candidates) == 0 || candidates[0].score == 0 ||
		candidates[0].score == candidates[1].score {
		return 0, sql.ErrNoRows
	}
	return candidates[0].rowID, nil
}

func samePersonFactEmploymentDate(left, right *PartialDate) bool {
	return left != nil && right != nil && CompareAtSharedPrecision(*left, *right) == 0
}

func personFactEmploymentDateRangesOverlap(employment *Employment, input EmploymentInput) bool {
	return employment.StartDate != nil && employment.EndDate != nil &&
		input.StartDate != nil && input.EndDate != nil &&
		CompareAtSharedPrecision(*employment.EndDate, *input.StartDate) >= 0 &&
		CompareAtSharedPrecision(*input.EndDate, *employment.StartDate) >= 0
}

func normalizedPersonFactEmploymentValue(
	target personfacts.TargetDescriptor, employment Employment, organization Organization,
) (*personfacts.NormalizedValue, error) {
	value := personfacts.EmploymentValue{
		Organization: canonicalPersonFactOrganizationReference(organization),
		Title:        personFactStringValue(employment.Title), Role: personFactStringValue(employment.Role),
		Department: personFactStringValue(employment.Department),
		Location:   personFactStringValue(employment.Location),
		StartDate:  personFactEmploymentPartialDate(employment.StartDate),
		EndDate:    personFactEmploymentPartialDate(employment.EndDate),
	}
	normalized, failure, err := personfacts.NormalizeClaimValue(target, mustMarshalPersonFactEmployment(value))
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, fmt.Errorf("normalize stored employment projection: %s", failure.Detail)
	}
	return normalized, nil
}

func canonicalPersonFactOrganizationReference(organization Organization) personfacts.OrganizationReference {
	ref := personfacts.OrganizationReference{ID: new(organization.ID), Name: organization.Name}
	if organization.PrimaryDomain != nil {
		ref.Domain = *organization.PrimaryDomain
	}
	return ref
}

func mustMarshalPersonFactEmployment(value personfacts.EmploymentValue) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func personFactEmploymentPartialDate(value *PartialDate) *personfacts.PartialDateValue {
	if value == nil || value.Year == nil {
		return nil
	}
	result := &personfacts.PartialDateValue{Year: *value.Year}
	if value.Month != nil {
		result.Month = *value.Month
	}
	if value.Day != nil {
		result.Day = *value.Day
	}
	return result
}

func storeEmploymentPartialDate(value *personfacts.PartialDateValue) *PartialDate {
	if value == nil {
		return nil
	}
	result := &PartialDate{Year: new(value.Year)}
	if value.Month != 0 {
		result.Month = new(value.Month)
	}
	if value.Day != 0 {
		result.Day = new(value.Day)
	}
	return result
}

func personFactEmploymentActiveFrom(employment Employment) time.Time {
	if employment.StartDate == nil || employment.StartDate.Year == nil {
		return employment.CreatedAt.UTC()
	}
	month, day := 1, 1
	if employment.StartDate.Month != nil {
		month = *employment.StartDate.Month
	}
	if employment.StartDate.Day != nil {
		day = *employment.StartDate.Day
	}
	return time.Date(*employment.StartDate.Year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

func personFactEmploymentProvenance(origin personfacts.ClaimOrigin) (Provenance, error) {
	switch origin {
	case personfacts.OriginExtraction:
		return ProvenanceExtraction, nil
	case personfacts.OriginEnrichment:
		return ProvenanceEnrichment, nil
	case personfacts.OriginSystem:
		return ProvenanceSystem, nil
	default:
		return "", fmt.Errorf("unsupported employment claim origin %q", origin)
	}
}

func personFactEmploymentInput(
	personID, organizationID int64, value personfacts.EmploymentValue,
	source Provenance, sourceRef string, confidence float64,
) EmploymentInput {
	current := value.EndDate == nil
	return EmploymentInput{
		PersonID: personID, OrganizationID: organizationID,
		Title: trimEmploymentString(&value.Title), Role: trimEmploymentString(&value.Role),
		Department: trimEmploymentString(&value.Department),
		Location:   trimEmploymentString(&value.Location),
		StartDate:  storeEmploymentPartialDate(value.StartDate),
		EndDate:    storeEmploymentPartialDate(value.EndDate),
		IsCurrent:  &current, Source: source, SourceRef: &sourceRef, Confidence: &confidence,
	}
}

func (p *personFactAttributeProjector) project(
	ctx context.Context, plan personfacts.ProjectionPlan, claim personfacts.ResolvedClaim,
	decisionKey string, transactionTime time.Time,
) (*personfacts.ProjectionRef, bool, error) {
	if claim.Normalized == nil {
		return nil, false, errors.New("attribute projection claim has no normalized value")
	}
	switch plan.Operation {
	case personfacts.ProjectionSet:
		if len(claim.Claim.Evidence) == 0 {
			return nil, false, errors.New("attribute projection claim has no evidence")
		}
		value, err := personFactAttributeValueFromNormalized(p.definition, *claim.Normalized)
		if err != nil {
			return nil, false, err
		}
		var source Provenance
		switch claim.Claim.Origin {
		case personfacts.OriginEnrichment:
			source = ProvenanceEnrichment
		case personfacts.OriginSystem:
			source = ProvenanceSystem
		default:
			source = ProvenanceExtraction
		}
		sourceRef := personFactDecisionSourceRefPrefix + decisionKey
		confidence := float64(claim.Claim.Confidence.ReportedScore) / 1000
		activeFrom := plan.ActiveFrom
		input := PersonAttributeValueInput{
			PersonID: claim.Claim.Evidence[0].PersonID, DefinitionSlug: p.definition.Slug,
			Value: value, ActiveFrom: &activeFrom, Source: source,
			SourceRef: &sourceRef, Confidence: &confidence,
		}
		if plan.CurrentRef != nil {
			current, err := p.loadCurrentValue(ctx, plan.CurrentRef.RowID)
			if err != nil {
				return nil, false, err
			}
			input.PersonID = current.PersonID
			input.Ordinal = &current.Ordinal
			input.ExpectedValueID = &current.ID
			if activeFrom.Before(current.ActiveFrom) {
				activeFrom = current.ActiveFrom
			}
		}
		write, err := p.store.setPersonAttributeValueTx(
			ctx, p.tx, p.definition, input, activeFrom, transactionTime)
		if err != nil {
			return nil, false, err
		}
		ref := &personfacts.ProjectionRef{Kind: "person_attribute", RowID: write.Value.ID}
		return ref, true, nil
	case personfacts.ProjectionRetire:
		if plan.CurrentRef == nil || plan.CurrentRef.Kind != "person_attribute" {
			return nil, false, errors.New("attribute retirement requires an exact current attribute reference")
		}
		current, err := p.loadCurrentValue(ctx, plan.CurrentRef.RowID)
		if err != nil {
			return nil, false, err
		}
		activeUntil := plan.ActiveFrom
		if activeUntil.Before(current.ActiveFrom) {
			activeUntil = current.ActiveFrom
		}
		if _, err := p.store.closePersonAttributeValueTx(
			ctx, p.tx, current.ID, activeUntil, transactionTime); err != nil {
			return nil, false, err
		}
		ref := *plan.CurrentRef
		return &ref, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported person fact projection operation %q", plan.Operation)
	}
}

func (p *personFactAttributeProjector) loadCurrentValue(
	ctx context.Context, rowID int64,
) (*PersonAttributeValue, error) {
	value, err := scanPersonAttributeValue(p.tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.id = ? AND v.definition_id = ?
		  AND v.active_until IS NULL AND v.superseded_at IS NULL%s
	`, personAttributeValueColumns, p.store.dialect.SelectForUpdate()), rowID, p.definition.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAttributeValueConflict
	}
	if err != nil {
		return nil, fmt.Errorf("load person fact current attribute %d: %w", rowID, err)
	}
	return value, nil
}

func (p *personFactAttributeProjector) retireUnsupported(
	ctx context.Context, personID int64, activeUntil, transactionTime time.Time,
	current []personfacts.CurrentProjection, claims []personfacts.ResolvedClaim,
	decisions []personfacts.Decision, planned map[personfacts.ProjectionRef]struct{},
) ([]personFactRetiredProjection, bool, error) {
	live := make(map[string]struct{})
	retired := make([]personFactRetiredProjection, 0)
	claimByKey := make(map[string]personfacts.ResolvedClaim, len(claims))
	for _, claim := range claims {
		claimByKey[claim.ClaimKey] = claim
	}
	for _, decision := range decisions {
		claim := claimByKey[decision.ClaimKey]
		if claim.Normalized == nil || claim.Claim.Relation != personfacts.RelationSupport ||
			decision.Reason == personfacts.ReasonEvidenceUnsupported ||
			decision.Reason == personfacts.ReasonOutsideValidity ||
			decision.Action == personfacts.DecisionInvalid ||
			decision.Action == personfacts.DecisionIdentityRejected ||
			decision.Action == personfacts.DecisionPolicyRejected {
			continue
		}
		live[claim.Normalized.Fingerprint] = struct{}{}
	}
	for _, item := range current {
		if item.Declared {
			continue
		}
		owned, err := ownsPersonFactProjectionTx(ctx, p.tx, personID, item.Ref)
		if err != nil {
			return nil, false, err
		}
		if !owned {
			continue
		}
		if _, exists := planned[item.Ref]; exists {
			continue
		}
		if _, exists := live[item.Normalized.Fingerprint]; exists {
			continue
		}
		currentValue, err := p.loadCurrentValue(ctx, item.Ref.RowID)
		if err != nil {
			return nil, false, err
		}
		claimKey := ""
		expiryClaimKey := ""
		var expiredAt *time.Time
		for _, decision := range decisions {
			claim := claimByKey[decision.ClaimKey]
			if claim.Normalized == nil || claim.Normalized.Fingerprint != item.Normalized.Fingerprint {
				continue
			}
			if decision.Reason == personfacts.ReasonEvidenceUnsupported {
				claimKey = decision.ClaimKey
				expiredAt = nil
				break
			}
			if decision.Reason != personfacts.ReasonOutsideValidity || claim.Claim.ValidUntil == nil ||
				activeUntil.Before(*claim.Claim.ValidUntil) {
				continue
			}
			validUntil := claim.Claim.ValidUntil.UTC()
			if expiredAt == nil || expiredAt.Before(validUntil) ||
				(expiredAt.Equal(validUntil) && decision.ClaimKey < expiryClaimKey) {
				expiryClaimKey = decision.ClaimKey
				expiredAt = &validUntil
			}
		}
		worldTime := activeUntil
		if expiredAt != nil {
			worldTime = *expiredAt
			claimKey = expiryClaimKey
		}
		if worldTime.Before(currentValue.ActiveFrom) {
			worldTime = currentValue.ActiveFrom
		}
		if _, err := p.store.closePersonAttributeValueTx(
			ctx, p.tx, currentValue.ID, worldTime, transactionTime); err != nil {
			return nil, false, err
		}
		retired = append(retired, personFactRetiredProjection{Ref: item.Ref, ClaimKey: claimKey})
	}
	return retired, len(retired) > 0, nil
}

const personFactAttributeProjectionOwnershipSQL = `
	SELECT EXISTS (
		SELECT 1 FROM person_fact_decisions
		WHERE person_id = ? AND projection_kind = ? AND projection_row_id = ?
	)`

const personFactEmploymentProjectionOwnershipSQL = `
	SELECT EXISTS (
		SELECT 1
		FROM employments e
		JOIN person_fact_decisions fd
		  ON fd.person_id = e.person_id
		 AND fd.projection_kind = 'employment'
		 AND fd.projection_row_id = e.id
		WHERE e.person_id = ? AND e.id = ?
		  AND e.source_ref = ? || fd.decision_key
	)`

func ownsPersonFactProjectionTx(
	ctx context.Context, tx *loggedTx, personID int64, ref personfacts.ProjectionRef,
) (bool, error) {
	var owned bool
	query := personFactAttributeProjectionOwnershipSQL
	args := []any{personID, ref.Kind, ref.RowID}
	if ref.Kind == personFactProjectionKindEmployment {
		query = personFactEmploymentProjectionOwnershipSQL
		args = []any{personID, ref.RowID, personFactDecisionSourceRefPrefix}
	}
	err := tx.QueryRowContext(ctx, query, args...).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("load person fact projection ownership: %w", err)
	}
	return owned, nil
}

func (s *Store) loadPersonFactTargetDescriptorTx(
	ctx context.Context, tx *loggedTx, kind personfacts.TargetKind, key string,
	fallback *personfacts.TargetDescriptor, allowSensitive bool,
) (personfacts.TargetDescriptor, personFactTargetEligibility, error) {
	return s.loadPersonFactTargetDescriptor(ctx, tx, kind, key, fallback, allowSensitive, true)
}

func (s *Store) loadPersonFactTargetDescriptorSnapshotTx(
	ctx context.Context, tx *loggedTx, kind personfacts.TargetKind, key string,
	fallback *personfacts.TargetDescriptor, allowSensitive bool,
) (personfacts.TargetDescriptor, personFactTargetEligibility, error) {
	return s.loadPersonFactTargetDescriptor(ctx, tx, kind, key, fallback, allowSensitive, false)
}

func (s *Store) loadPersonFactTargetDescriptor(
	ctx context.Context, tx *loggedTx, kind personfacts.TargetKind, key string,
	fallback *personfacts.TargetDescriptor, allowSensitive, lock bool,
) (personfacts.TargetDescriptor, personFactTargetEligibility, error) {
	switch kind {
	case personfacts.TargetAttribute:
		definition, err := s.getAttributeDefinitionByUniversalID(ctx, tx, key, lock)
		if errors.Is(err, ErrAttributeDefinitionNotFound) {
			if fallback != nil {
				return *fallback, personFactTargetEligibility{}, nil
			}
			return personfacts.TargetDescriptor{
				Kind: personfacts.TargetAttribute, Key: key, UniversalID: key,
				Slug: key, Description: "Unavailable attribute target",
				ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
				Revision: "unavailable",
			}, personFactTargetEligibility{}, nil
		}
		if err != nil {
			return personfacts.TargetDescriptor{}, personFactTargetEligibility{}, err
		}
		descriptor, err := personFactAttributeDescriptor(*definition)
		if err != nil {
			return personfacts.TargetDescriptor{}, personFactTargetEligibility{}, err
		}
		eligibleWithSensitive := personFactAttributeEligible(*definition, true)
		return descriptor, personFactTargetEligibility{
			Supported: personFactAttributeEligible(*definition, allowSensitive),
			SensitivePolicyBlocked: eligibleWithSensitive && definition.IsSensitive &&
				!allowSensitive,
		}, nil
	case personfacts.TargetEmployment:
		catalog, err := personfacts.BuildCatalog(nil, personfacts.CatalogOptions{})
		if err != nil {
			return personfacts.TargetDescriptor{}, personFactTargetEligibility{}, err
		}
		for _, descriptor := range catalog.Targets {
			if descriptor.Kind == kind && descriptor.Key == key {
				return descriptor, personFactTargetEligibility{Supported: true}, nil
			}
		}
	}
	if fallback != nil {
		return *fallback, personFactTargetEligibility{}, nil
	}
	return personfacts.TargetDescriptor{}, personFactTargetEligibility{},
		fmt.Errorf("unknown person fact target %s/%s", kind, key)
}

func (s *Store) getAttributeDefinitionByUniversalID(
	ctx context.Context, tx *loggedTx, universalID string, lock bool,
) (*AttributeDefinition, error) {
	lockClause := ""
	if lock {
		lockClause = s.dialect.SelectForUpdate()
	}
	definition, err := scanAttributeDefinition(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM attribute_definitions
		WHERE object_type = ? AND universal_id = ?%s
	`, attributeDefinitionColumns, lockClause),
		string(AttributeObjectPerson), universalID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAttributeDefinitionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person attribute definition %q: %w", universalID, err)
	}
	return definition, nil
}

func personFactAttributeDescriptor(definition AttributeDefinition) (personfacts.TargetDescriptor, error) {
	descriptor := personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: definition.UniversalID,
		UniversalID: definition.UniversalID, Slug: definition.Slug,
		Description:  strings.TrimSpace(personFactStringValue(definition.Description)),
		ValueType:    personfacts.ValueType(definition.ValueType),
		Cardinality:  personfacts.Cardinality(definition.Cardinality),
		RecordTarget: personFactStringValue(definition.RecordTarget),
		MaxLength:    personFactMaxLength(definition.Options),
		Choices:      personFactChoices(definition.Options), Sensitive: definition.IsSensitive,
	}
	revision, err := personfacts.DescriptorRevision(descriptor)
	if err != nil {
		return personfacts.TargetDescriptor{}, err
	}
	descriptor.Revision = revision
	return descriptor, nil
}

func personFactAttributeEligible(definition AttributeDefinition, allowSensitive bool) bool {
	if !definition.IsActive || !definition.APIMutable || definition.DerivedSource != nil ||
		definition.RecordTarget != nil || (definition.IsSensitive && !allowSensitive) {
		return false
	}
	description := strings.TrimSpace(personFactStringValue(definition.Description))
	if description == "" || len([]rune(description)) > 280 {
		return false
	}
	switch definition.ValueType {
	case AttributeValueText, AttributeValueInteger, AttributeValueReal,
		AttributeValueBoolean, AttributeValueDate, AttributeValueTimestamp:
		return true
	default:
		return false
	}
}

func normalizedPersonFactAttributeValue(
	target personfacts.TargetDescriptor, value AttributeValue,
) (*personfacts.NormalizedValue, error) {
	raw, err := personFactAttributeValueJSON(value)
	if err != nil {
		return nil, err
	}
	normalized, failure, err := personfacts.NormalizeClaimValue(target, raw)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, fmt.Errorf("normalize stored attribute projection: %s", failure.Detail)
	}
	return normalized, nil
}

func personFactAttributeValueJSON(value AttributeValue) (json.RawMessage, error) {
	var scalar any
	switch value.Type {
	case AttributeValueText:
		scalar = value.Text
	case AttributeValueInteger:
		scalar = value.Integer
	case AttributeValueReal:
		scalar = value.Real
	case AttributeValueBoolean:
		scalar = value.Boolean
	case AttributeValueDate:
		scalar = value.Date
	case AttributeValueTimestamp:
		if value.Timestamp != nil {
			scalar = value.Timestamp.UTC().Format(time.RFC3339Nano)
		}
	default:
		return nil, fmt.Errorf("attribute value type %q is not a generic person fact", value.Type)
	}
	encoded, err := json.Marshal(scalar)
	if err != nil {
		return nil, fmt.Errorf("encode stored person fact attribute: %w", err)
	}
	return encoded, nil
}

func personFactAttributeValueFromNormalized(
	definition AttributeDefinition, normalized personfacts.NormalizedValue,
) (AttributeValue, error) {
	value := AttributeValue{Type: definition.ValueType}
	var err error
	switch definition.ValueType {
	case AttributeValueText:
		err = json.Unmarshal(normalized.JSON, &value.Text)
	case AttributeValueInteger:
		err = json.Unmarshal(normalized.JSON, &value.Integer)
	case AttributeValueReal:
		err = json.Unmarshal(normalized.JSON, &value.Real)
	case AttributeValueBoolean:
		err = json.Unmarshal(normalized.JSON, &value.Boolean)
	case AttributeValueDate:
		err = json.Unmarshal(normalized.JSON, &value.Date)
	case AttributeValueTimestamp:
		var rendered string
		if err = json.Unmarshal(normalized.JSON, &rendered); err == nil {
			var parsed time.Time
			parsed, err = time.Parse(time.RFC3339Nano, rendered)
			if err == nil {
				parsed = parsed.UTC()
				value.Timestamp = &parsed
			}
		}
	default:
		return AttributeValue{}, fmt.Errorf("unsupported person fact attribute type %q", definition.ValueType)
	}
	if err != nil {
		return AttributeValue{}, fmt.Errorf("decode normalized person fact attribute: %w", err)
	}
	return value, nil
}

func sortPersonFactProjectionPlans(plans []personfacts.ProjectionPlan) {
	sort.Slice(plans, func(i, j int) bool {
		left, right := plans[i], plans[j]
		if left.Target.Kind != right.Target.Kind {
			return left.Target.Kind < right.Target.Kind
		}
		if left.Target.Key != right.Target.Key {
			return left.Target.Key < right.Target.Key
		}
		if left.Operation != right.Operation {
			return left.Operation < right.Operation
		}
		if left.ClaimKey != right.ClaimKey {
			return left.ClaimKey < right.ClaimKey
		}
		return personFactProjectionRowID(left.CurrentRef) < personFactProjectionRowID(right.CurrentRef)
	})
}

func sortPersonFactProjectionRefs(refs []personfacts.ProjectionRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].RowID < refs[j].RowID
	})
}

func personFactProjectionRowID(ref *personfacts.ProjectionRef) int64 {
	if ref == nil {
		return 0
	}
	return ref.RowID
}

func personFactTargetMapKey(kind personfacts.TargetKind, key string) string {
	return string(kind) + "\x00" + key
}

func copyPersonFactNormalized(value *personfacts.NormalizedValue) *personfacts.NormalizedValue {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.JSON = append(json.RawMessage(nil), value.JSON...)
	return &copyValue
}

func copyPersonFactFailure(value *personfacts.ValidationFailure) *personfacts.ValidationFailure {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
