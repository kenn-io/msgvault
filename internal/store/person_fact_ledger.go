package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// ErrPersonFactKeyCollision reports that a deterministic key already names
// different immutable ledger content.
var ErrPersonFactKeyCollision = errors.New("person fact deterministic key collision")

type personFactLedgerGeneration struct {
	Generation           personfacts.Generation
	Claims               []personfacts.Claim
	Evidence             []personfacts.Evidence
	EvidenceStatusEvents []personfacts.EvidenceStatusEvent
	Resolutions          []personfacts.ResolutionResult
}

func (s *Store) insertPersonFactGenerationTx(
	ctx context.Context,
	tx *loggedTx,
	prepared personfacts.PreparedGeneration,
) (personfacts.Generation, bool, error) {
	input := prepared.Input()
	input.ResolvedAt = personFactPortableTime(input.ResolvedAt)
	claims := prepared.Claims()
	statuses := prepared.EvidenceStatusChanges()
	if input.PersonID <= 0 || len(prepared.CanonicalJSON()) == 0 || prepared.GenerationKey() == "" {
		return personfacts.Generation{}, false, errors.New("prepared person fact generation is empty")
	}
	key, err := personfacts.GenerationKey(input, claims, statuses)
	if err != nil {
		return personfacts.Generation{}, false, fmt.Errorf("verify prepared person fact generation: %w", err)
	}
	if key != prepared.GenerationKey() || input.ProgramFingerprint != prepared.ProgramFingerprint() {
		return personfacts.Generation{}, false, errors.New("prepared person fact generation failed integrity verification")
	}
	cursorsJSON, err := json.Marshal(input.SourceCursors)
	if err != nil {
		return personfacts.Generation{}, false, fmt.Errorf("encode person fact source cursors: %w", err)
	}

	var insertedID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO person_fact_generations
			(person_id, generation_key, source_cursors_json, program_id, program_version,
			 program_fingerprint, catalog_fingerprint, provider, provider_version,
			 model, model_version, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		input.PersonID, prepared.GenerationKey(), string(cursorsJSON), input.ProgramID,
		input.ProgramVersion, input.ProgramFingerprint, input.CatalogFingerprint,
		input.Provider, input.ProviderVersion, input.Model, input.ModelVersion,
		input.Policy.ProviderPolicyFingerprint, input.ResolvedAt,
	).Scan(&insertedID)
	replay := false
	if errors.Is(err, sql.ErrNoRows) {
		replay = true
	} else if err != nil {
		return personfacts.Generation{}, false, fmt.Errorf("insert person fact generation: %w", err)
	}

	generation, err := s.loadPersonFactGenerationRowTx(ctx, tx, input.PersonID, prepared.GenerationKey())
	if err != nil {
		return personfacts.Generation{}, false, err
	}
	if insertedID != 0 && generation.ID != insertedID {
		return personfacts.Generation{}, false, errors.New("insert person fact generation returned inconsistent id")
	}
	if !personFactGenerationMatches(generation, input, prepared.GenerationKey()) {
		return personfacts.Generation{}, false, personFactCollision("generation", prepared.GenerationKey())
	}
	return generation, replay, nil
}

func (s *Store) insertPersonFactClaimsTx(
	ctx context.Context,
	tx *loggedTx,
	generation personfacts.Generation,
	preparedClaims []personfacts.PreparedClaim,
) ([]personfacts.Claim, error) {
	return s.insertPersonFactClaimsWithFailuresTx(ctx, tx, generation, preparedClaims, nil)
}

func (s *Store) insertPersonFactClaimsWithFailuresTx(
	ctx context.Context,
	tx *loggedTx,
	generation personfacts.Generation,
	preparedClaims []personfacts.PreparedClaim,
	failures map[string]*personfacts.ValidationFailure,
) ([]personfacts.Claim, error) {
	claims := make([]personfacts.Claim, 0, len(preparedClaims))
	for _, prepared := range preparedClaims {
		claimKey, err := personfacts.ClaimKey(generation.GenerationKey, prepared)
		if err != nil {
			return nil, fmt.Errorf("compute person fact claim key: %w", err)
		}
		persisted := prepared
		if failure := failures[claimKey]; failure != nil {
			persisted.Normalized = nil
			persisted.Failure = copyPersonFactFailure(failure)
		}
		claim, err := s.insertPersonFactClaimWithKeyTx(ctx, tx, generation, claimKey, persisted)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ClaimKey < claims[j].ClaimKey })
	return claims, nil
}

func (s *Store) insertPersonFactClaimWithKeyTx(
	ctx context.Context,
	tx *loggedTx,
	generation personfacts.Generation,
	claimKey string,
	prepared personfacts.PreparedClaim,
) (personfacts.Claim, error) {
	prepared.ValidFrom = personFactPortableTimePointer(prepared.ValidFrom)
	prepared.ValidUntil = personFactPortableTimePointer(prepared.ValidUntil)
	confidenceJSON, err := json.Marshal(prepared.Confidence)
	if err != nil {
		return personfacts.Claim{}, fmt.Errorf("encode person fact claim confidence: %w", err)
	}
	var normalizedJSON, valueFingerprint any
	if prepared.Normalized != nil {
		normalizedJSON = string(prepared.Normalized.JSON)
		valueFingerprint = prepared.Normalized.Fingerprint
	}
	var rejectionAction, rejectionReason, rejectionDetail any
	if prepared.Failure != nil {
		rejectionAction = prepared.Failure.Action
		rejectionReason = prepared.Failure.Reason
		rejectionDetail = prepared.Failure.Detail
	}

	var insertedID int64
	err = tx.QueryRowContext(ctx, `
			INSERT INTO person_fact_claims
				(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
				 relation, submitted_value_json, normalized_value_json, value_fingerprint,
				 valid_from, valid_until, origin, confidence_json,
				 rejection_action, rejection_reason, rejection_detail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		generation.PersonID, generation.ID, claimKey, prepared.Target.Kind, prepared.Target.Key,
		prepared.Target.Revision, prepared.Relation, string(prepared.SubmittedValue), normalizedJSON,
		valueFingerprint, personFactOptionalTime(prepared.ValidFrom), personFactOptionalTime(prepared.ValidUntil),
		prepared.Origin, string(confidenceJSON), rejectionAction, rejectionReason, rejectionDetail,
	).Scan(&insertedID)
	inserted := true
	if errors.Is(err, sql.ErrNoRows) {
		inserted = false
	} else if err != nil {
		return personfacts.Claim{}, fmt.Errorf("insert person fact claim: %w", err)
	}

	evidenceIDs := make([]int64, 0, len(prepared.Evidence))
	for index, evidenceInput := range prepared.Evidence {
		evidenceKey := prepared.EvidenceKeys[index]
		evidence, insertErr := s.insertPersonFactEvidenceTx(ctx, tx, generation.PersonID,
			evidenceKey, evidenceInput)
		if insertErr != nil {
			return personfacts.Claim{}, insertErr
		}
		evidenceIDs = append(evidenceIDs, evidence.ID)
	}
	slices.Sort(evidenceIDs)

	claim, err := s.loadPersonFactClaimRowTx(ctx, tx, generation, claimKey)
	if err != nil {
		return personfacts.Claim{}, err
	}
	if insertedID != 0 && claim.ID != insertedID {
		return personfacts.Claim{}, errors.New("insert person fact claim returned inconsistent id")
	}
	if !personFactClaimMatches(claim, prepared, generation, claimKey, evidenceIDs) {
		return personfacts.Claim{}, personFactCollision("claim", claimKey)
	}
	if inserted {
		for _, evidenceID := range evidenceIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO person_fact_claim_evidence (claim_id, evidence_id)
				VALUES (?, ?) ON CONFLICT DO NOTHING`, claim.ID, evidenceID); err != nil {
				return personfacts.Claim{}, fmt.Errorf("link person fact claim evidence: %w", err)
			}
		}
		claim.EvidenceIDs = append([]int64(nil), evidenceIDs...)
		return claim, nil
	}
	storedEvidenceIDs, err := loadPersonFactClaimEvidenceIDsTx(ctx, tx, claim.ID)
	if err != nil {
		return personfacts.Claim{}, err
	}
	if !reflect.DeepEqual(storedEvidenceIDs, evidenceIDs) {
		return personfacts.Claim{}, personFactCollision("claim evidence", claimKey)
	}
	claim.EvidenceIDs = storedEvidenceIDs
	return claim, nil
}

func (s *Store) insertPersonFactEvidenceTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	evidenceKey string,
	input personfacts.EvidenceInput,
) (personfacts.Evidence, error) {
	input.EventTime = personFactPortableTime(input.EventTime)
	input.RecordedTime = personFactPortableTime(input.RecordedTime)
	computedKey, err := personfacts.EvidenceKey(input)
	if err != nil {
		return personfacts.Evidence{}, fmt.Errorf("verify person fact evidence: %w", err)
	}
	if computedKey != evidenceKey || input.PersonID != personID {
		return personfacts.Evidence{}, errors.New("prepared person fact evidence failed integrity verification")
	}
	var insertedID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO person_fact_evidence
			(person_id, evidence_key, source_class, directness, authority, source_ref,
			 source_url, subject_person_id, subject_ref, span_start, span_end, excerpt,
			 content_sha256, source_version, event_time, recorded_time, identity_score)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		personID, evidenceKey, input.SourceClass, input.Directness, input.Authority,
		input.SourceRef, input.SourceURL, personFactOptionalInt64(input.SubjectPersonID), input.SubjectRef,
		personFactOptionalInt64(input.SpanStart), personFactOptionalInt64(input.SpanEnd), input.Excerpt,
		input.ContentSHA256, input.SourceVersion, input.EventTime, input.RecordedTime,
		input.IdentityScore,
	).Scan(&insertedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return personfacts.Evidence{}, fmt.Errorf("insert person fact evidence: %w", err)
	}
	evidence, err := s.loadPersonFactEvidenceRowTx(ctx, tx, personID, evidenceKey)
	if err != nil {
		return personfacts.Evidence{}, err
	}
	if insertedID != 0 && evidence.ID != insertedID {
		return personfacts.Evidence{}, errors.New("insert person fact evidence returned inconsistent id")
	}
	if !personFactEvidenceInputMatches(evidence.Input, input) {
		return personfacts.Evidence{}, personFactCollision("evidence", evidenceKey)
	}
	return evidence, nil
}

func (s *Store) insertPersonFactEvidenceStatusEventsTx(
	ctx context.Context,
	tx *loggedTx,
	generation personfacts.Generation,
	changes []personfacts.PreparedEvidenceStatusChange,
) error {
	for _, change := range changes {
		evidence, err := s.loadPersonFactEvidenceRowTx(ctx, tx, generation.PersonID, change.EvidenceKey)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("person fact status references unknown evidence %q", change.EvidenceKey)
			}
			return err
		}
		if evidence.Input.SourceVersion != change.SourceVersion {
			return fmt.Errorf("person fact status source version does not match evidence %q", change.EvidenceKey)
		}
		var insertedID int64
		err = tx.QueryRowContext(ctx, `
			INSERT INTO person_fact_evidence_status_events
				(person_id, generation_id, evidence_id, evidence_key, source_version,
				 supported, reason)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING
			RETURNING id`,
			generation.PersonID, generation.ID, evidence.ID, change.EvidenceKey,
			change.SourceVersion, change.Supported, change.Reason,
		).Scan(&insertedID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("insert person fact evidence status: %w", err)
		}
		event, loadErr := loadPersonFactStatusEventByScopedKeyTx(ctx, tx, generation,
			change.EvidenceKey, change.SourceVersion, change.Reason)
		if loadErr != nil {
			return loadErr
		}
		if insertedID != 0 && event.ID != insertedID {
			return errors.New("insert person fact evidence status returned inconsistent id")
		}
		if event.PersonID != generation.PersonID || event.GenerationID != generation.ID ||
			event.EvidenceID != evidence.ID || event.EvidenceKey != change.EvidenceKey ||
			event.SourceVersion != change.SourceVersion || event.Supported != change.Supported ||
			event.Reason != change.Reason {
			return personFactCollision("evidence status", change.EvidenceKey)
		}
	}
	return nil
}

func (s *Store) insertPersonFactResolutionTx(
	ctx context.Context,
	tx *loggedTx,
	generation personfacts.Generation,
	resolution personfacts.Resolution,
	providerPolicyFingerprint string,
) (personfacts.ResolutionResult, error) {
	resolution.ResolvedAt = personFactPortableTime(resolution.ResolvedAt)
	var insertedID int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO person_fact_resolutions
			(person_id, generation_id, target_kind, target_key, target_revision,
			 resolver_version, input_fingerprint, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		generation.PersonID, generation.ID, resolution.Target.Kind, resolution.Target.Key,
		resolution.Target.Revision, resolution.ResolverVersion, resolution.InputFingerprint,
		providerPolicyFingerprint, resolution.ResolvedAt,
	).Scan(&insertedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return personfacts.ResolutionResult{}, fmt.Errorf("insert person fact resolution: %w", err)
	}
	stored, storedPolicy, err := s.loadPersonFactResolutionShellTx(ctx, tx, generation.ID,
		resolution.Target, resolution.ResolverVersion, resolution.InputFingerprint)
	if err != nil {
		return personfacts.ResolutionResult{}, err
	}
	if insertedID != 0 && stored.ID != insertedID {
		return personfacts.ResolutionResult{}, errors.New("insert person fact resolution returned inconsistent id")
	}
	if stored.Target != resolution.Target || stored.ResolverVersion != resolution.ResolverVersion ||
		stored.InputFingerprint != resolution.InputFingerprint ||
		!personFactPortableTime(stored.ResolvedAt).Equal(resolution.ResolvedAt) ||
		storedPolicy != providerPolicyFingerprint {
		return personfacts.ResolutionResult{}, personFactCollision("resolution", resolution.InputFingerprint)
	}
	for _, decision := range resolution.Decisions {
		if err := s.insertPersonFactDecisionTx(ctx, tx, generation.PersonID, stored.ID,
			resolution.InputFingerprint, decision); err != nil {
			return personfacts.ResolutionResult{}, err
		}
	}
	stored, err = s.hydratePersonFactResolutionTx(ctx, tx, stored)
	if err != nil {
		return personfacts.ResolutionResult{}, err
	}
	if len(stored.Decisions) != len(resolution.Decisions) {
		return personfacts.ResolutionResult{}, personFactCollision("resolution decisions", resolution.InputFingerprint)
	}
	return stored, nil
}

func (s *Store) insertPersonFactDecisionTx(
	ctx context.Context,
	tx *loggedTx,
	personID, resolutionID int64,
	resolutionFingerprint string,
	decision personfacts.Decision,
) error {
	if decision.ClaimKey == "" {
		return errors.New("person fact decision claim key is required")
	}
	decisionKey, err := personfacts.DecisionKey(resolutionFingerprint, decision.ClaimKey, decision.Action)
	if err != nil {
		return fmt.Errorf("compute person fact decision key: %w", err)
	}
	claimID, err := loadPersonFactClaimIDByKeyTx(ctx, tx, personID, decision.ClaimKey)
	if err != nil {
		return fmt.Errorf("load person fact decision claim: %w", err)
	}
	var competingClaimID any
	if decision.CompetingClaimKey != "" {
		id, loadErr := loadPersonFactClaimIDByKeyTx(ctx, tx, personID, decision.CompetingClaimKey)
		if loadErr != nil {
			return fmt.Errorf("load competing person fact claim: %w", loadErr)
		}
		competingClaimID = id
	}
	scoreJSON, err := json.Marshal(decision.Score)
	if err != nil {
		return fmt.Errorf("encode person fact decision score: %w", err)
	}
	var projectionKind, projectionRowID, resolvedOrganizationID any
	if decision.Projection != nil {
		projectionKind, projectionRowID = decision.Projection.Kind, decision.Projection.RowID
		if decision.Projection.Kind == personFactProjectionKindEmployment {
			var organizationID int64
			loadErr := tx.QueryRowContext(ctx, `
				SELECT organization_id FROM employments
				WHERE id = ? AND person_id = ?
			`, decision.Projection.RowID, personID).Scan(&organizationID)
			if errors.Is(loadErr, sql.ErrNoRows) {
				loadErr = tx.QueryRowContext(ctx, `
					SELECT resolved_organization_id
					FROM person_fact_decisions
					WHERE person_id = ? AND projection_kind = 'employment'
					  AND projection_row_id = ? AND resolved_organization_id IS NOT NULL
					ORDER BY id DESC LIMIT 1`,
					personID, decision.Projection.RowID).Scan(&organizationID)
			}
			if loadErr != nil {
				return fmt.Errorf("load resolved employment organization: %w", loadErr)
			}
			resolvedOrganizationID = organizationID
		}
	}
	var insertedID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO person_fact_decisions
			(person_id, resolution_id, claim_id, decision_key, action, reason, score_json,
			 competing_claim_id, projection_kind, projection_row_id, resolved_organization_id)
		VALUES (?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		personID, resolutionID, claimID, decisionKey, decision.Action, decision.Reason,
		string(scoreJSON), competingClaimID, projectionKind, projectionRowID,
		resolvedOrganizationID,
	).Scan(&insertedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("insert person fact decision: %w", err)
	}
	stored, err := loadPersonFactDecisionByKeyTx(ctx, tx, personID, decisionKey)
	if err != nil {
		return err
	}
	if insertedID != 0 && stored.ID != insertedID {
		return errors.New("insert person fact decision returned inconsistent id")
	}
	want := decision
	want.PersonID, want.ResolutionID, want.DecisionKey = personID, resolutionID, decisionKey
	if !personFactDecisionMatches(stored, want) {
		return personFactCollision("decision", decisionKey)
	}
	return nil
}

func (s *Store) loadPersonFactGenerationTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	generationKey string,
) (personFactLedgerGeneration, error) {
	generation, err := s.loadPersonFactGenerationRowTx(ctx, tx, personID, generationKey)
	if err != nil {
		return personFactLedgerGeneration{}, err
	}
	claims, err := s.listPersonFactClaimsTx(ctx, tx, personID,
		personfacts.ClaimFilter{}, "c.generation_id = ?", generation.ID, 0, 0)
	if err != nil {
		return personFactLedgerGeneration{}, err
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ClaimKey < claims[j].ClaimKey })
	evidence, err := s.listPersonFactGenerationEvidenceTx(ctx, tx, personID, generation.ID)
	if err != nil {
		return personFactLedgerGeneration{}, err
	}
	if err := s.hydratePersonFactEvidenceStatusTx(ctx, tx, evidence); err != nil {
		return personFactLedgerGeneration{}, err
	}
	statuses, err := s.listPersonFactStatusEventsTx(ctx, tx, personID,
		personfacts.EvidenceStatusFilter{}, "e.generation_id = ?", generation.ID, 0, 0)
	if err != nil {
		return personFactLedgerGeneration{}, err
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	resolutions, err := s.listPersonFactGenerationResolutionsTx(ctx, tx, generation.ID)
	if err != nil {
		return personFactLedgerGeneration{}, err
	}
	return personFactLedgerGeneration{
		Generation: generation, Claims: claims, Evidence: evidence,
		EvidenceStatusEvents: statuses, Resolutions: resolutions,
	}, nil
}

// ListPersonFactEvidenceContext returns a bounded newest-first evidence page
// with effective support hydrated from the latest immutable status event.
func (s *Store) ListPersonFactEvidenceContext(
	ctx context.Context,
	personID int64,
	filter personfacts.EvidenceFilter,
) ([]personfacts.Evidence, error) {
	limit, offset, err := personFactPage(filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	evidence := make([]personfacts.Evidence, 0)
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		evidence, err = s.queryPersonFactEvidenceTx(ctx, tx, personID,
			personfacts.EvidenceFilter{Target: filter.Target, Limit: limit, Offset: offset})
		if err != nil {
			return err
		}
		return s.hydratePersonFactEvidenceStatusTx(ctx, tx, evidence)
	})
	if err != nil {
		return nil, err
	}
	return evidence, nil
}

// ListPersonFactEvidenceStatusEventsContext returns the bounded append-only
// status-event audit stream in newest-first order.
func (s *Store) ListPersonFactEvidenceStatusEventsContext(
	ctx context.Context,
	personID int64,
	filter personfacts.EvidenceStatusFilter,
) ([]personfacts.EvidenceStatusEvent, error) {
	limit, offset, err := personFactPage(filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	events := make([]personfacts.EvidenceStatusEvent, 0)
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		events, err = s.listPersonFactStatusEventsTx(ctx, tx, personID, filter, "", nil, limit, offset)
		return err
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// ListPersonFactClaimsContext returns a bounded newest-first claim page.
func (s *Store) ListPersonFactClaimsContext(
	ctx context.Context,
	personID int64,
	filter personfacts.ClaimFilter,
) ([]personfacts.Claim, error) {
	limit, offset, err := personFactPage(filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	claims := make([]personfacts.Claim, 0)
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		claims, err = s.listPersonFactClaimsTx(ctx, tx, personID, filter, "", nil, limit, offset)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// ListPersonFactDecisionsContext returns a bounded newest-first decision page.
func (s *Store) ListPersonFactDecisionsContext(
	ctx context.Context,
	personID int64,
	filter personfacts.DecisionFilter,
) ([]personfacts.Decision, error) {
	limit, offset, err := personFactPage(filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	decisions := make([]personfacts.Decision, 0)
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		decisions, err = s.listPersonFactDecisionsTx(ctx, tx, personID, filter, limit, offset)
		return err
	})
	if err != nil {
		return nil, err
	}
	return decisions, nil
}

func (s *Store) queryPersonFactEvidenceTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	filter personfacts.EvidenceFilter,
) ([]personfacts.Evidence, error) {
	query := `SELECT ` + personFactEvidenceColumns + `
		FROM person_fact_evidence e WHERE e.person_id = ?`
	args := []any{personID}
	if filter.Target != nil {
		query += ` AND EXISTS (
			SELECT 1 FROM person_fact_claim_evidence ce
			JOIN person_fact_claims c ON c.id = ce.claim_id
			WHERE ce.evidence_id = e.id AND c.person_id = e.person_id
			  AND c.target_kind = ? AND c.target_key = ? AND c.target_revision = ?)`
		args = append(args, filter.Target.Kind, filter.Target.Key, filter.Target.Revision)
	}
	query += ` ORDER BY e.id DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list person fact evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	evidence := make([]personfacts.Evidence, 0)
	for rows.Next() {
		item, scanErr := scanPersonFactEvidence(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact evidence: %w", scanErr)
		}
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact evidence: %w", err)
	}
	return evidence, nil
}

func (s *Store) listPersonFactGenerationEvidenceTx(
	ctx context.Context,
	tx *loggedTx,
	personID, generationID int64,
) ([]personfacts.Evidence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+personFactEvidenceColumns+`
		FROM person_fact_evidence e
		WHERE e.person_id = ? AND EXISTS (
			SELECT 1 FROM person_fact_claim_evidence ce
			JOIN person_fact_claims c ON c.id = ce.claim_id
			WHERE ce.evidence_id = e.id AND c.generation_id = ?)
		ORDER BY e.evidence_key, e.id`, personID, generationID)
	if err != nil {
		return nil, fmt.Errorf("list person fact generation evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	evidence := make([]personfacts.Evidence, 0)
	for rows.Next() {
		item, scanErr := scanPersonFactEvidence(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact generation evidence: %w", scanErr)
		}
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact generation evidence: %w", err)
	}
	return evidence, nil
}

func (s *Store) hydratePersonFactEvidenceStatusTx(
	ctx context.Context,
	tx *loggedTx,
	evidence []personfacts.Evidence,
) error {
	for index := range evidence {
		evidence[index].Supported = true
		row := tx.QueryRowContext(ctx, `
			SELECT latest.id, latest.person_id, latest.generation_id, latest.evidence_id,
			       latest.evidence_key, latest.source_version, latest.supported,
			       latest.reason, latest.created_at
			FROM person_fact_evidence_status_events latest
			JOIN person_fact_generations generation ON generation.id = latest.generation_id
			WHERE latest.person_id = ? AND latest.evidence_key = ? AND latest.source_version = ?
			ORDER BY generation.resolved_at DESC, latest.id DESC
			LIMIT 1
		`, evidence[index].PersonID, evidence[index].Key, evidence[index].Input.SourceVersion)
		event, err := scanPersonFactStatusEvent(row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("hydrate latest person fact evidence status: %w", err)
		}
		evidence[index].Supported = event.Supported
		evidence[index].LatestStatus = &event
	}
	return nil
}

func (s *Store) listPersonFactStatusEventsTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	filter personfacts.EvidenceStatusFilter,
	extraPredicate string,
	extraArg any,
	limit, offset int,
) ([]personfacts.EvidenceStatusEvent, error) {
	query := `SELECT id, person_id, generation_id, evidence_id, evidence_key,
		       source_version, supported, reason, created_at
		FROM person_fact_evidence_status_events e WHERE e.person_id = ?`
	args := []any{personID}
	if filter.EvidenceKey != "" {
		query += ` AND e.evidence_key = ?`
		args = append(args, filter.EvidenceKey)
	}
	if filter.Supported != nil {
		query += ` AND e.supported = ?`
		args = append(args, *filter.Supported)
	}
	if extraPredicate != "" {
		query += ` AND ` + extraPredicate
		args = append(args, extraArg)
	}
	query += ` ORDER BY e.id DESC`
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list person fact evidence statuses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events := make([]personfacts.EvidenceStatusEvent, 0)
	for rows.Next() {
		event, scanErr := scanPersonFactStatusEvent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact evidence status: %w", scanErr)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact evidence statuses: %w", err)
	}
	return events, nil
}

func (s *Store) listPersonFactClaimsTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	filter personfacts.ClaimFilter,
	extraPredicate string,
	extraArg any,
	limit, offset int,
) ([]personfacts.Claim, error) {
	query := `SELECT ` + personFactClaimColumns + `
		FROM person_fact_claims c WHERE c.person_id = ?`
	args := []any{personID}
	if filter.Target != nil {
		query += ` AND c.target_kind = ? AND c.target_key = ? AND c.target_revision = ?`
		args = append(args, filter.Target.Kind, filter.Target.Key, filter.Target.Revision)
	}
	if extraPredicate != "" {
		query += ` AND ` + extraPredicate
		args = append(args, extraArg)
	}
	query += ` ORDER BY c.id DESC`
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list person fact claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	claims := make([]personfacts.Claim, 0)
	for rows.Next() {
		claim, scanErr := scanPersonFactClaim(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact claim: %w", scanErr)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact claims: %w", err)
	}
	for index := range claims {
		generation, loadErr := s.loadPersonFactGenerationByIDTx(ctx, tx, claims[index].GenerationID)
		if loadErr != nil {
			return nil, loadErr
		}
		claims[index].Generation = generation
		claims[index].EvidenceIDs, loadErr = loadPersonFactClaimEvidenceIDsTx(ctx, tx, claims[index].ID)
		if loadErr != nil {
			return nil, loadErr
		}
	}
	return claims, nil
}

func (s *Store) listPersonFactDecisionsTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	filter personfacts.DecisionFilter,
	limit, offset int,
) ([]personfacts.Decision, error) {
	query := `SELECT ` + personFactDecisionColumns + `
		FROM person_fact_decisions d
		JOIN person_fact_resolutions r ON r.id = d.resolution_id
		LEFT JOIN person_fact_claims c ON c.id = d.claim_id
		LEFT JOIN person_fact_claims competing ON competing.id = d.competing_claim_id
		WHERE d.person_id = ?`
	args := []any{personID}
	if filter.Target != nil {
		query += ` AND r.target_kind = ? AND r.target_key = ? AND r.target_revision = ?`
		args = append(args, filter.Target.Kind, filter.Target.Key, filter.Target.Revision)
	}
	query += ` ORDER BY d.id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list person fact decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	decisions := make([]personfacts.Decision, 0)
	for rows.Next() {
		decision, scanErr := scanPersonFactDecision(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person fact decision: %w", scanErr)
		}
		decisions = append(decisions, decision)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact decisions: %w", err)
	}
	return decisions, nil
}

const personFactEvidenceColumns = `e.id, e.person_id, e.evidence_key,
	e.source_class, e.directness, e.authority, e.source_ref, e.source_url,
	e.subject_person_id, e.subject_ref, e.span_start, e.span_end, e.excerpt,
	e.content_sha256, e.source_version, e.event_time, e.recorded_time,
	e.identity_score, e.created_at`

func scanPersonFactEvidence(row scanner) (personfacts.Evidence, error) {
	return scanPersonFactEvidenceWithPrefix(row)
}

func scanPersonFactEvidenceWithPrefix(row scanner, prefix ...any) (personfacts.Evidence, error) {
	var evidence personfacts.Evidence
	var subjectID, spanStart, spanEnd sql.NullInt64
	var eventTime, recordedTime, createdAt personFactTimestamp
	destinations := make([]any, len(prefix), len(prefix)+19)
	copy(destinations, prefix)
	destinations = append(destinations,
		&evidence.ID, &evidence.PersonID, &evidence.Key,
		&evidence.Input.SourceClass, &evidence.Input.Directness, &evidence.Input.Authority,
		&evidence.Input.SourceRef, &evidence.Input.SourceURL, &subjectID, &evidence.Input.SubjectRef,
		&spanStart, &spanEnd, &evidence.Input.Excerpt, &evidence.Input.ContentSHA256,
		&evidence.Input.SourceVersion, &eventTime, &recordedTime, &evidence.Input.IdentityScore,
		&createdAt,
	)
	err := row.Scan(destinations...)
	if err != nil {
		return personfacts.Evidence{}, err
	}
	evidence.Input.PersonID = evidence.PersonID
	evidence.Input.SubjectPersonID = personFactNullInt64(subjectID)
	evidence.Input.SpanStart = personFactNullInt64(spanStart)
	evidence.Input.SpanEnd = personFactNullInt64(spanEnd)
	evidence.Input.EventTime = eventTime.Time
	evidence.Input.RecordedTime = recordedTime.Time
	evidence.CreatedAt = createdAt.Time
	evidence.Supported = true
	return evidence, nil
}

const personFactClaimColumns = `c.id, c.person_id, c.generation_id, c.claim_key,
	c.target_kind, c.target_key, c.target_revision, c.relation,
	c.submitted_value_json, CAST(c.normalized_value_json AS TEXT), c.value_fingerprint,
	c.valid_from, c.valid_until, c.origin, CAST(c.confidence_json AS TEXT),
	c.rejection_action, c.rejection_reason, c.rejection_detail, c.created_at`

func scanPersonFactClaim(row scanner) (personfacts.Claim, error) {
	var claim personfacts.Claim
	var submitted string
	var normalizedJSON, valueFingerprint sql.NullString
	var validFrom, validUntil nullableTimestamp
	var confidenceJSON string
	var rejectionAction, rejectionReason, rejectionDetail sql.NullString
	var createdAt personFactTimestamp
	err := row.Scan(
		&claim.ID, &claim.PersonID, &claim.GenerationID, &claim.ClaimKey,
		&claim.Target.Kind, &claim.Target.Key, &claim.Target.Revision, &claim.Relation,
		&submitted, &normalizedJSON, &valueFingerprint, &validFrom, &validUntil,
		&claim.Origin, &confidenceJSON, &rejectionAction, &rejectionReason, &rejectionDetail, &createdAt,
	)
	if err != nil {
		return personfacts.Claim{}, err
	}
	claim.SubmittedValue = append(json.RawMessage(nil), submitted...)
	if normalizedJSON.Valid && valueFingerprint.Valid {
		claim.Normalized = &personfacts.NormalizedValue{
			JSON: append(json.RawMessage(nil), normalizedJSON.String...), Fingerprint: valueFingerprint.String,
		}
	}
	if validFrom.Valid {
		value := validFrom.Time.UTC()
		claim.ValidFrom = &value
	}
	if validUntil.Valid {
		value := validUntil.Time.UTC()
		claim.ValidUntil = &value
	}
	if err := json.Unmarshal([]byte(confidenceJSON), &claim.Confidence); err != nil {
		return personfacts.Claim{}, fmt.Errorf("decode confidence: %w", err)
	}
	if rejectionAction.Valid && rejectionReason.Valid && rejectionDetail.Valid {
		claim.Failure = &personfacts.ValidationFailure{
			Action: personfacts.DecisionAction(rejectionAction.String),
			Reason: personfacts.DecisionReason(rejectionReason.String),
			Detail: rejectionDetail.String,
		}
	}
	claim.CreatedAt = createdAt.Time
	claim.EvidenceIDs = make([]int64, 0)
	return claim, nil
}

func scanPersonFactStatusEvent(row scanner) (personfacts.EvidenceStatusEvent, error) {
	var event personfacts.EvidenceStatusEvent
	var createdAt personFactTimestamp
	err := row.Scan(&event.ID, &event.PersonID, &event.GenerationID, &event.EvidenceID,
		&event.EvidenceKey, &event.SourceVersion, &event.Supported, &event.Reason, &createdAt)
	if err != nil {
		return personfacts.EvidenceStatusEvent{}, err
	}
	event.CreatedAt = createdAt.Time
	return event, nil
}

const personFactDecisionColumns = `d.id, d.person_id, d.resolution_id, d.decision_key,
	COALESCE(c.claim_key, ''), d.action, d.reason, CAST(d.score_json AS TEXT),
	COALESCE(competing.claim_key, ''), d.projection_kind, d.projection_row_id, d.created_at`

func scanPersonFactDecision(row scanner) (personfacts.Decision, error) {
	var decision personfacts.Decision
	var scoreJSON string
	var projectionKind sql.NullString
	var projectionRowID sql.NullInt64
	var createdAt personFactTimestamp
	err := row.Scan(
		&decision.ID, &decision.PersonID, &decision.ResolutionID, &decision.DecisionKey,
		&decision.ClaimKey, &decision.Action, &decision.Reason, &scoreJSON,
		&decision.CompetingClaimKey, &projectionKind, &projectionRowID, &createdAt,
	)
	if err != nil {
		return personfacts.Decision{}, err
	}
	if err := json.Unmarshal([]byte(scoreJSON), &decision.Score); err != nil {
		return personfacts.Decision{}, fmt.Errorf("decode score: %w", err)
	}
	if projectionKind.Valid && projectionRowID.Valid {
		decision.Projection = &personfacts.ProjectionRef{Kind: projectionKind.String, RowID: projectionRowID.Int64}
	}
	decision.CreatedAt = createdAt.Time
	return decision, nil
}

func (s *Store) loadPersonFactGenerationRowTx(
	ctx context.Context, tx *loggedTx, personID int64, generationKey string,
) (personfacts.Generation, error) {
	return scanPersonFactGeneration(tx.QueryRowContext(ctx, `
		SELECT id, person_id, generation_key, CAST(source_cursors_json AS TEXT),
		       program_id, program_version, program_fingerprint, catalog_fingerprint,
		       provider, provider_version, model, model_version,
		       provider_policy_fingerprint, resolved_at, created_at
		FROM person_fact_generations WHERE person_id = ? AND generation_key = ?`,
		personID, generationKey))
}

func (s *Store) loadPersonFactGenerationByIDTx(
	ctx context.Context, tx *loggedTx, generationID int64,
) (personfacts.Generation, error) {
	generation, err := scanPersonFactGeneration(tx.QueryRowContext(ctx, `
		SELECT id, person_id, generation_key, CAST(source_cursors_json AS TEXT),
		       program_id, program_version, program_fingerprint, catalog_fingerprint,
		       provider, provider_version, model, model_version,
		       provider_policy_fingerprint, resolved_at, created_at
		FROM person_fact_generations WHERE id = ?`, generationID))
	if err != nil {
		return personfacts.Generation{}, fmt.Errorf("load person fact generation: %w", err)
	}
	return generation, nil
}

func scanPersonFactGeneration(row scanner) (personfacts.Generation, error) {
	var generation personfacts.Generation
	var cursorsJSON string
	var resolvedAt, createdAt personFactTimestamp
	err := row.Scan(
		&generation.ID, &generation.PersonID, &generation.GenerationKey, &cursorsJSON,
		&generation.ProgramID, &generation.ProgramVersion, &generation.ProgramFingerprint,
		&generation.CatalogFingerprint, &generation.Provider, &generation.ProviderVersion,
		&generation.Model, &generation.ModelVersion, &generation.Policy.ProviderPolicyFingerprint,
		&resolvedAt, &createdAt,
	)
	if err != nil {
		return personfacts.Generation{}, err
	}
	if err := json.Unmarshal([]byte(cursorsJSON), &generation.SourceCursors); err != nil {
		return personfacts.Generation{}, fmt.Errorf("decode source cursors: %w", err)
	}
	if generation.SourceCursors == nil {
		generation.SourceCursors = make([]personfacts.SourceCursor, 0)
	}
	// AllowSensitive is execution input bound by GenerationKey, not durable
	// consent state. The ledger intentionally hydrates only the stored policy
	// fingerprint; callers must not interpret the zero bool as reconstructed
	// consent posture.
	generation.ResolvedAt = resolvedAt.Time
	generation.CreatedAt = createdAt.Time
	return generation, nil
}

func (s *Store) loadPersonFactEvidenceRowTx(
	ctx context.Context, tx *loggedTx, personID int64, evidenceKey string,
) (personfacts.Evidence, error) {
	evidence, err := scanPersonFactEvidence(tx.QueryRowContext(ctx,
		`SELECT `+personFactEvidenceColumns+` FROM person_fact_evidence e
		 WHERE e.person_id = ? AND e.evidence_key = ?`, personID, evidenceKey))
	if err != nil {
		return personfacts.Evidence{}, fmt.Errorf("load person fact evidence: %w", err)
	}
	return evidence, nil
}

func (s *Store) loadPersonFactClaimRowTx(
	ctx context.Context, tx *loggedTx, generation personfacts.Generation, claimKey string,
) (personfacts.Claim, error) {
	claim, err := scanPersonFactClaim(tx.QueryRowContext(ctx,
		`SELECT `+personFactClaimColumns+` FROM person_fact_claims c
		 WHERE c.person_id = ? AND c.claim_key = ?`, generation.PersonID, claimKey))
	if err != nil {
		return personfacts.Claim{}, fmt.Errorf("load person fact claim: %w", err)
	}
	claim.Generation = generation
	return claim, nil
}

func loadPersonFactStatusEventByScopedKeyTx(
	ctx context.Context,
	tx *loggedTx,
	generation personfacts.Generation,
	evidenceKey, sourceVersion string,
	reason personfacts.EvidenceStatusReason,
) (personfacts.EvidenceStatusEvent, error) {
	event, err := scanPersonFactStatusEvent(tx.QueryRowContext(ctx, `
		SELECT id, person_id, generation_id, evidence_id, evidence_key,
		       source_version, supported, reason, created_at
		FROM person_fact_evidence_status_events
		WHERE person_id = ? AND generation_id = ? AND evidence_key = ?
		  AND source_version = ? AND reason = ?`, generation.PersonID, generation.ID,
		evidenceKey, sourceVersion, reason))
	if err != nil {
		return personfacts.EvidenceStatusEvent{}, fmt.Errorf("load person fact evidence status: %w", err)
	}
	return event, nil
}

func (s *Store) loadPersonFactResolutionShellTx(
	ctx context.Context,
	tx *loggedTx,
	generationID int64,
	target personfacts.TargetRef,
	resolverVersion, inputFingerprint string,
) (personfacts.ResolutionResult, string, error) {
	var result personfacts.ResolutionResult
	var policy string
	var resolvedAt personFactTimestamp
	err := tx.QueryRowContext(ctx, `
		SELECT id, target_kind, target_key, target_revision, resolver_version,
		       input_fingerprint, provider_policy_fingerprint, resolved_at
		FROM person_fact_resolutions
		WHERE generation_id = ? AND target_kind = ? AND target_key = ?
		  AND resolver_version = ? AND input_fingerprint = ?`,
		generationID, target.Kind, target.Key, resolverVersion, inputFingerprint,
	).Scan(&result.ID, &result.Target.Kind, &result.Target.Key, &result.Target.Revision,
		&result.ResolverVersion, &result.InputFingerprint, &policy, &resolvedAt)
	if err != nil {
		return personfacts.ResolutionResult{}, "", fmt.Errorf("load person fact resolution: %w", err)
	}
	result.ResolvedAt = resolvedAt.Time
	result.Decisions = make([]personfacts.Decision, 0)
	result.Projections = make([]personfacts.ProjectionRef, 0)
	return result, policy, nil
}

func (s *Store) listPersonFactGenerationResolutionsTx(
	ctx context.Context, tx *loggedTx, generationID int64,
) ([]personfacts.ResolutionResult, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, target_kind, target_key, target_revision, resolver_version,
		       input_fingerprint, resolved_at
		FROM person_fact_resolutions WHERE generation_id = ?
		ORDER BY target_kind, target_key, target_revision, id`, generationID)
	if err != nil {
		return nil, fmt.Errorf("list person fact resolutions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	results := make([]personfacts.ResolutionResult, 0)
	for rows.Next() {
		var result personfacts.ResolutionResult
		var resolvedAt personFactTimestamp
		if err := rows.Scan(&result.ID, &result.Target.Kind, &result.Target.Key,
			&result.Target.Revision, &result.ResolverVersion, &result.InputFingerprint,
			&resolvedAt); err != nil {
			return nil, fmt.Errorf("scan person fact resolution: %w", err)
		}
		result.ResolvedAt = resolvedAt.Time
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact resolutions: %w", err)
	}
	for index := range results {
		results[index], err = s.hydratePersonFactResolutionTx(ctx, tx, results[index])
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (s *Store) hydratePersonFactResolutionTx(
	ctx context.Context, tx *loggedTx, result personfacts.ResolutionResult,
) (personfacts.ResolutionResult, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+personFactDecisionColumns+`
		FROM person_fact_decisions d
		LEFT JOIN person_fact_claims c ON c.id = d.claim_id
		LEFT JOIN person_fact_claims competing ON competing.id = d.competing_claim_id
		WHERE d.resolution_id = ?
		ORDER BY COALESCE(c.claim_key, ''), d.action, d.id`, result.ID)
	if err != nil {
		return personfacts.ResolutionResult{}, fmt.Errorf("hydrate person fact resolution decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result.Decisions = make([]personfacts.Decision, 0)
	projectionSet := make(map[personfacts.ProjectionRef]struct{})
	for rows.Next() {
		decision, scanErr := scanPersonFactDecision(rows)
		if scanErr != nil {
			return personfacts.ResolutionResult{}, fmt.Errorf("scan person fact resolution decision: %w", scanErr)
		}
		result.Decisions = append(result.Decisions, decision)
		if decision.Projection != nil {
			projectionSet[*decision.Projection] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return personfacts.ResolutionResult{}, fmt.Errorf("iterate person fact resolution decisions: %w", err)
	}
	result.Projections = make([]personfacts.ProjectionRef, 0, len(projectionSet))
	for projection := range projectionSet {
		result.Projections = append(result.Projections, projection)
	}
	sort.Slice(result.Projections, func(i, j int) bool {
		if result.Projections[i].Kind != result.Projections[j].Kind {
			return result.Projections[i].Kind < result.Projections[j].Kind
		}
		return result.Projections[i].RowID < result.Projections[j].RowID
	})
	return result, nil
}

func loadPersonFactDecisionByKeyTx(
	ctx context.Context, tx *loggedTx, personID int64, decisionKey string,
) (personfacts.Decision, error) {
	decision, err := scanPersonFactDecision(tx.QueryRowContext(ctx, `
		SELECT `+personFactDecisionColumns+`
		FROM person_fact_decisions d
		LEFT JOIN person_fact_claims c ON c.id = d.claim_id
		LEFT JOIN person_fact_claims competing ON competing.id = d.competing_claim_id
		WHERE d.person_id = ? AND d.decision_key = ?`, personID, decisionKey))
	if err != nil {
		return personfacts.Decision{}, fmt.Errorf("load person fact decision: %w", err)
	}
	return decision, nil
}

func loadPersonFactClaimIDByKeyTx(
	ctx context.Context, tx *loggedTx, personID int64, claimKey string,
) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM person_fact_claims WHERE person_id = ? AND claim_key = ?`,
		personID, claimKey).Scan(&id)
	return id, err
}

func loadPersonFactClaimEvidenceIDsTx(
	ctx context.Context, tx *loggedTx, claimID int64,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT evidence_id FROM person_fact_claim_evidence
		WHERE claim_id = ? ORDER BY evidence_id`, claimID)
	if err != nil {
		return nil, fmt.Errorf("load person fact claim evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan person fact claim evidence: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person fact claim evidence: %w", err)
	}
	return ids, nil
}

func personFactGenerationMatches(
	stored personfacts.Generation, input personfacts.GenerationInput, generationKey string,
) bool {
	return stored.PersonID == input.PersonID && stored.GenerationKey == generationKey &&
		reflect.DeepEqual(stored.SourceCursors, input.SourceCursors) &&
		stored.ProgramID == input.ProgramID && stored.ProgramVersion == input.ProgramVersion &&
		stored.ProgramFingerprint == input.ProgramFingerprint &&
		stored.CatalogFingerprint == input.CatalogFingerprint &&
		stored.Provider == input.Provider && stored.ProviderVersion == input.ProviderVersion &&
		stored.Model == input.Model && stored.ModelVersion == input.ModelVersion &&
		stored.Policy.ProviderPolicyFingerprint == input.Policy.ProviderPolicyFingerprint
}

func personFactClaimMatches(
	stored personfacts.Claim,
	prepared personfacts.PreparedClaim,
	generation personfacts.Generation,
	claimKey string,
	evidenceIDs []int64,
) bool {
	if stored.PersonID != generation.PersonID || stored.GenerationID != generation.ID ||
		stored.ClaimKey != claimKey || stored.Target != (personfacts.TargetRef{
		Kind: prepared.Target.Kind, Key: prepared.Target.Key, Revision: prepared.Target.Revision,
	}) || stored.Relation != prepared.Relation || stored.Origin != prepared.Origin ||
		!personFactRawJSONEqual(stored.SubmittedValue, prepared.SubmittedValue) ||
		!personFactNormalizedEqual(stored.Normalized, prepared.Normalized) ||
		!personFactTimePointerEqual(stored.ValidFrom, prepared.ValidFrom) ||
		!personFactTimePointerEqual(stored.ValidUntil, prepared.ValidUntil) ||
		stored.Confidence != prepared.Confidence ||
		!reflect.DeepEqual(stored.Failure, prepared.Failure) {
		return false
	}
	if len(stored.EvidenceIDs) > 0 && !reflect.DeepEqual(stored.EvidenceIDs, evidenceIDs) {
		return false
	}
	return true
}

func personFactEvidenceInputMatches(left, right personfacts.EvidenceInput) bool {
	return left.PersonID == right.PersonID && left.SourceClass == right.SourceClass &&
		left.Directness == right.Directness && left.Authority == right.Authority &&
		left.SourceRef == right.SourceRef && left.SourceURL == right.SourceURL &&
		personFactInt64PointerEqual(left.SubjectPersonID, right.SubjectPersonID) &&
		left.SubjectRef == right.SubjectRef &&
		personFactInt64PointerEqual(left.SpanStart, right.SpanStart) &&
		personFactInt64PointerEqual(left.SpanEnd, right.SpanEnd) &&
		left.Excerpt == right.Excerpt && left.ContentSHA256 == right.ContentSHA256 &&
		left.SourceVersion == right.SourceVersion &&
		personFactPortableTime(left.EventTime).Equal(personFactPortableTime(right.EventTime)) &&
		personFactPortableTime(left.RecordedTime).Equal(personFactPortableTime(right.RecordedTime)) &&
		left.IdentityScore == right.IdentityScore
}

func personFactDecisionMatches(left, right personfacts.Decision) bool {
	return left.PersonID == right.PersonID && left.ResolutionID == right.ResolutionID &&
		left.DecisionKey == right.DecisionKey && left.ClaimKey == right.ClaimKey &&
		left.Action == right.Action && left.Reason == right.Reason &&
		left.Score == right.Score && left.CompetingClaimKey == right.CompetingClaimKey &&
		reflect.DeepEqual(left.Projection, right.Projection)
}

func personFactRawJSONEqual(left, right []byte) bool {
	if json.Valid(left) && json.Valid(right) {
		return equalJSON(left, right)
	}
	return bytes.Equal(left, right)
}

func personFactNormalizedEqual(left, right *personfacts.NormalizedValue) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Fingerprint == right.Fingerprint && personFactRawJSONEqual(left.JSON, right.JSON)
}

func personFactPage(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return 0, 0, errors.New("person fact page limit must be between 1 and 200")
	}
	if offset < 0 {
		return 0, 0, errors.New("person fact page offset must not be negative")
	}
	return limit, offset, nil
}

func personFactCollision(kind, key string) error {
	return fmt.Errorf("%w: %s %q", ErrPersonFactKeyCollision, kind, key)
}

func personFactOptionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func personFactOptionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func personFactNullInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func personFactInt64PointerEqual(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func personFactTimePointerEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return personFactPortableTime(*left).Equal(personFactPortableTime(*right))
}

func personFactPortableTime(input time.Time) time.Time {
	return input.UTC().Truncate(time.Microsecond)
}

func personFactPortableTimePointer(input *time.Time) *time.Time {
	if input == nil {
		return nil
	}
	value := personFactPortableTime(*input)
	return &value
}

type personFactTimestamp struct{ Time time.Time }

func (value *personFactTimestamp) Scan(source any) error {
	var scanned nullableTimestamp
	if err := scanned.Scan(source); err != nil {
		return err
	}
	if !scanned.Valid {
		return errors.New("person fact timestamp is null or invalid")
	}
	value.Time = scanned.Time.UTC()
	return nil
}
