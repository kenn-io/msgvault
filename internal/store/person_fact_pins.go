package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

func (s *Store) SetPersonFactPinContext(
	ctx context.Context, personID int64, target personfacts.TargetRef,
	pinned bool, actor string,
) (*personfacts.PinWrite, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, errors.New("person fact pin actor is required")
	}
	return retryContendedWrite(ctx, s, "set person fact pin",
		func() (*personfacts.PinWrite, error) {
			return s.setPersonFactPinOnce(ctx, personID, target, pinned, actor)
		})
}

func (s *Store) setPersonFactPinOnce(
	ctx context.Context, personID int64, target personfacts.TargetRef,
	pinned bool, actor string,
) (*personfacts.PinWrite, error) {
	write := &personfacts.PinWrite{
		Resolutions: []personfacts.ResolutionResult{}, Projections: []personfacts.ProjectionRef{},
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "person-fact-generation", personID); err != nil {
			return err
		}
		if err := verifyTrackedPersonFactPersonTx(ctx, tx, personID); err != nil {
			return err
		}
		descriptor, eligibility, err := s.loadPersonFactTargetDescriptorTx(
			ctx, tx, target.Kind, target.Key, nil, true)
		if err != nil {
			return err
		}
		if !eligibility.Supported {
			return fmt.Errorf("person fact target %s/%s is not active", target.Kind, target.Key)
		}
		currentRef := personfacts.TargetRef{
			Kind: descriptor.Kind, Key: descriptor.Key, Revision: descriptor.Revision,
		}
		if target != currentRef {
			return errors.New("person fact pin target descriptor is stale")
		}
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "person-fact-target", personID, target.Kind, target.Key); err != nil {
			return err
		}
		projector, err := s.personFactProjectorTx(ctx, tx, descriptor)
		if err != nil {
			return err
		}
		current, err := projector.loadCurrent(ctx, personID, descriptor)
		if err != nil {
			return err
		}
		state, err := s.effectivePersonFactPinTx(ctx, tx, personID, descriptor, current)
		if err != nil {
			return err
		}
		if state.Pinned == pinned {
			write.State = state
			return nil
		}
		state, err = s.appendPersonFactPinEventTx(ctx, tx, personID, currentRef, pinned, actor)
		if err != nil {
			return err
		}
		write.State = state

		policy := personfacts.PolicyContext{AllowSensitive: descriptor.Sensitive}
		generationID, exists, err := latestPersonFactTargetGenerationIDTx(
			ctx, tx, personID, target.Kind, target.Key)
		if err != nil {
			return err
		}
		if exists {
			latest, loadErr := s.loadPersonFactGenerationByIDTx(ctx, tx, generationID)
			if loadErr != nil {
				return loadErr
			}
			policy.ProviderPolicyFingerprint = latest.Policy.ProviderPolicyFingerprint
		}
		actionTime := time.Now().UTC()
		catalog, err := s.buildPersonFactCatalogContext(ctx, tx, descriptor.Sensitive)
		if err != nil {
			return err
		}
		prepared, err := preparePersonFactPinGeneration(
			ctx, personID, descriptor, state, policy, catalog.Fingerprint, actionTime)
		if err != nil {
			return err
		}
		generation, replay, err := s.insertPersonFactGenerationTx(ctx, tx, prepared)
		if err != nil {
			return err
		}
		if replay {
			return errors.New("person fact pin generation unexpectedly replayed for a new pin event")
		}
		changed, err := s.resolvePersonFactTargetTx(
			ctx, tx, generation, descriptor, eligibility, policy, nil, actionTime)
		if err != nil {
			return err
		}
		if changed {
			if err := s.bumpPersonVCardProjectionsTx(ctx, tx, personID); err != nil {
				return err
			}
		}
		result, err := s.loadPersonFactGenerationResultTx(ctx, tx, personID, generation.GenerationKey)
		if err != nil {
			return err
		}
		for _, resolution := range result.Resolutions {
			if resolution.Target.Kind == target.Kind && resolution.Target.Key == target.Key {
				write.Resolutions = append(write.Resolutions, resolution)
			}
		}
		if len(write.Resolutions) > 1 {
			write.Resolutions = write.Resolutions[len(write.Resolutions)-1:]
		}
		projectionSet := make(map[personfacts.ProjectionRef]struct{})
		for _, resolution := range write.Resolutions {
			for _, projection := range resolution.Projections {
				projectionSet[projection] = struct{}{}
			}
		}
		for projection := range projectionSet {
			write.Projections = append(write.Projections, projection)
		}
		sortPersonFactProjectionRefs(write.Projections)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return write, nil
}

func preparePersonFactPinGeneration(
	ctx context.Context, personID int64, target personfacts.TargetDescriptor,
	state personfacts.PinState, policy personfacts.PolicyContext,
	catalogFingerprint string, resolvedAt time.Time,
) (personfacts.PreparedGeneration, error) {
	if state.EventID == nil {
		return personfacts.PreparedGeneration{}, errors.New("person fact pin generation requires a durable pin event")
	}
	programFingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte("msgvault-person-fact-pin/v1")))
	return personfacts.PreparePersonFactGeneration(ctx, personfacts.GenerationInput{
		PersonID: personID,
		SourceCursors: []personfacts.SourceCursor{{
			Lane:  "person-fact-pin:" + string(target.Kind) + ":" + target.Key,
			Start: fmt.Sprintf("event:%d", *state.EventID),
			End:   fmt.Sprintf("pinned:%t@%s", state.Pinned, target.Revision),
		}},
		ProgramID:          "msgvault-person-fact-pin",
		ProgramVersion:     "v1",
		ProgramFingerprint: programFingerprint,
		CatalogFingerprint: catalogFingerprint,
		Provider:           "msgvault",
		ProviderVersion:    "person-fact-pin-v1",
		ResolvedAt:         resolvedAt,
		Policy:             policy,
	}, nil)
}

const personFactPinAttributeTargetCandidatesSQL = `
	SELECT d.universal_id
	FROM person_attribute_values v
	JOIN attribute_definitions d ON d.id = v.definition_id
	WHERE v.person_id = ? AND v.active_until IS NULL AND v.superseded_at IS NULL
	  AND d.is_active = TRUE AND d.api_mutable = TRUE
	  AND d.derived_source IS NULL AND d.record_target IS NULL
	  AND LENGTH(TRIM(COALESCE(d.description, ''))) BETWEEN 1 AND 280
	  AND d.value_type IN ('text', 'integer', 'real', 'boolean', 'date', 'timestamp')
	  AND (v.source IN (?, ?, ?)
	    OR NOT EXISTS (
	      SELECT 1
	      FROM person_fact_decisions fd
	      WHERE fd.person_id = v.person_id
	        AND fd.projection_kind = ?
	        AND fd.projection_row_id = v.id
	    ))
	ORDER BY v.definition_id, v.ordinal`

const personFactPinEmploymentTargetCandidateSQL = `
	SELECT EXISTS (
		SELECT 1 FROM employments e
		WHERE e.person_id = ? AND e.source IN (?, ?, ?)
	) OR EXISTS (
		SELECT 1 FROM employments e
		WHERE e.person_id = ? AND e.is_current = TRUE
		  AND NOT EXISTS (
		    SELECT 1
		    FROM person_fact_decisions fd
		    WHERE fd.person_id = e.person_id
		      AND fd.projection_kind = ?
		      AND fd.projection_row_id = e.id
		      AND e.source_ref = ? || fd.decision_key
		  )
		)`

func (s *Store) ListPersonFactPinsContext(
	ctx context.Context, personID int64,
) ([]personfacts.PinState, error) {
	states := make([]personfacts.PinState, 0)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var personExists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM persons WHERE id = ?`, personID).Scan(&personExists); err != nil {
			return fmt.Errorf("verify person %d for fact pins: %w", personID, err)
		}
		if personExists == 0 {
			return ErrPersonNotFound
		}
		targetKeys := make(map[string]personFactTouchedTarget)
		latestEvents := make(map[string]personfacts.PinState)
		rows, err := tx.QueryContext(ctx, `
			SELECT latest.id, latest.target_kind, latest.target_key,
			       latest.target_revision, latest.pinned, latest.actor
			FROM person_fact_pin_events latest
			WHERE latest.person_id = ? AND latest.id = (
				SELECT MAX(candidate.id) FROM person_fact_pin_events candidate
				WHERE candidate.person_id = latest.person_id
				  AND candidate.target_kind = latest.target_kind
				  AND candidate.target_key = latest.target_key)
			ORDER BY latest.target_kind, latest.target_key`, personID)
		if err != nil {
			return fmt.Errorf("list person fact pin targets: %w", err)
		}
		for rows.Next() {
			var target personFactTouchedTarget
			var state personfacts.PinState
			var eventID int64
			if scanErr := rows.Scan(&eventID, &target.Kind, &target.Key,
				&state.Target.Revision, &state.Pinned, &state.Actor); scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("scan person fact pin target: %w", scanErr)
			}
			state.Target.Kind = target.Kind
			state.Target.Key = target.Key
			state.EventID = &eventID
			mapKey := personFactTargetMapKey(target.Kind, target.Key)
			targetKeys[mapKey] = target
			latestEvents[mapKey] = state
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate person fact pin targets: %w", rowsErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return fmt.Errorf("close person fact pin targets: %w", closeErr)
		}

		attributeRows, err := tx.QueryContext(ctx, personFactPinAttributeTargetCandidatesSQL, personID,
			ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport, "person_attribute")
		if err != nil {
			return fmt.Errorf("list protected person fact pin targets: %w", err)
		}
		for attributeRows.Next() {
			var key string
			if scanErr := attributeRows.Scan(&key); scanErr != nil {
				_ = attributeRows.Close()
				return fmt.Errorf("scan protected person fact pin target: %w", scanErr)
			}
			mapKey := personFactTargetMapKey(personfacts.TargetAttribute, key)
			if _, exists := targetKeys[mapKey]; !exists {
				targetKeys[mapKey] = personFactTouchedTarget{Kind: personfacts.TargetAttribute, Key: key}
			}
		}
		if rowsErr := attributeRows.Err(); rowsErr != nil {
			_ = attributeRows.Close()
			return fmt.Errorf("iterate protected person fact pin targets: %w", rowsErr)
		}
		if closeErr := attributeRows.Close(); closeErr != nil {
			return fmt.Errorf("close protected person fact pin targets: %w", closeErr)
		}
		var hasProtectedEmployment bool
		if err := tx.QueryRowContext(ctx, personFactPinEmploymentTargetCandidateSQL,
			personID, ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport,
			personID, "employment", personFactDecisionSourceRefPrefix,
		).Scan(&hasProtectedEmployment); err != nil {
			return fmt.Errorf("list protected employment fact pin target: %w", err)
		}
		if hasProtectedEmployment {
			target := personFactTouchedTarget{
				Kind: personfacts.TargetEmployment, Key: "system:employment",
			}
			targetKeys[personFactTargetMapKey(target.Kind, target.Key)] = target
		}

		for _, target := range targetKeys {
			descriptor, _, loadErr := s.loadPersonFactTargetDescriptorSnapshotTx(
				ctx, tx, target.Kind, target.Key, nil, true)
			if loadErr != nil {
				return loadErr
			}
			projector, loadErr := s.personFactProjectorSnapshotTx(ctx, tx, descriptor)
			if errors.Is(loadErr, ErrAttributeDefinitionNotFound) {
				if state, exists := latestEvents[personFactTargetMapKey(target.Kind, target.Key)]; exists {
					states = append(states, state)
					continue
				}
			}
			if loadErr != nil {
				return loadErr
			}
			current, loadErr := projector.loadCurrent(ctx, personID, descriptor)
			if loadErr != nil {
				return loadErr
			}
			state, loadErr := s.effectivePersonFactPinTx(ctx, tx, personID, descriptor, current)
			if loadErr != nil {
				return loadErr
			}
			states = append(states, state)
		}
		sort.Slice(states, func(i, j int) bool {
			if states[i].Target.Kind != states[j].Target.Kind {
				return states[i].Target.Kind < states[j].Target.Kind
			}
			return states[i].Target.Key < states[j].Target.Key
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return states, nil
}

func (s *Store) appendManualPersonFactAttributePinTx(
	ctx context.Context, tx *loggedTx, definition AttributeDefinition,
	personID int64, actor string,
) error {
	if !personFactAttributePinSupported(definition) {
		return nil
	}
	target, err := personFactAttributeTargetRef(definition)
	if err != nil {
		return err
	}
	_, err = s.appendPersonFactPinEventTx(ctx, tx, personID, target, true, actor)
	return err
}

func personFactAttributePinSupported(definition AttributeDefinition) bool {
	if !definition.APIMutable || definition.DerivedSource != nil || definition.RecordTarget != nil {
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

func (s *Store) appendPersonFactPinEventTx(
	ctx context.Context, tx *loggedTx, personID int64, target personfacts.TargetRef,
	pinned bool, actor string,
) (personfacts.PinState, error) {
	var eventID int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO person_fact_pin_events
			(person_id, target_kind, target_key, target_revision, pinned, actor)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id`, personID, target.Kind, target.Key, target.Revision, pinned, actor,
	).Scan(&eventID)
	if err != nil {
		return personfacts.PinState{}, fmt.Errorf("append person fact pin event: %w", err)
	}
	return personfacts.PinState{
		Target: target, Pinned: pinned, Actor: actor, EventID: &eventID,
	}, nil
}

func (s *Store) effectivePersonFactPinTx(
	ctx context.Context, tx *loggedTx, personID int64,
	target personfacts.TargetDescriptor, current []personfacts.CurrentProjection,
) (personfacts.PinState, error) {
	state := personfacts.PinState{Target: personfacts.TargetRef{
		Kind: target.Kind, Key: target.Key, Revision: target.Revision,
	}}
	var eventID int64
	var eventRevision string
	err := tx.QueryRowContext(ctx, `
		SELECT id, target_revision, pinned, actor
		FROM person_fact_pin_events
		WHERE person_id = ? AND target_kind = ? AND target_key = ?
		ORDER BY id DESC LIMIT 1`, personID, target.Kind, target.Key,
	).Scan(&eventID, &eventRevision, &state.Pinned, &state.Actor)
	if err == nil {
		state.EventID = &eventID
		return state, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return personfacts.PinState{}, fmt.Errorf("load effective person fact pin: %w", err)
	}
	if target.Kind == personfacts.TargetEmployment {
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM employments
				WHERE person_id = ? AND source IN (?, ?, ?)
			)`, personID, ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport,
		).Scan(&state.Pinned); err != nil {
			return personfacts.PinState{}, fmt.Errorf("load declared employment fact pin: %w", err)
		}
	} else {
		for _, projection := range current {
			if projection.Declared {
				state.Pinned = true
				break
			}
		}
	}
	if state.Pinned {
		return state, nil
	}
	for _, projection := range current {
		owned, ownershipErr := ownsPersonFactProjectionTx(
			ctx, tx, personID, projection.Ref)
		if ownershipErr != nil {
			return personfacts.PinState{}, ownershipErr
		}
		if !owned {
			state.Pinned = true
			break
		}
	}
	return state, nil
}

func latestPersonFactTargetGenerationIDTx(
	ctx context.Context, tx *loggedTx, personID int64,
	kind personfacts.TargetKind, key string,
) (int64, bool, error) {
	var generationID int64
	err := tx.QueryRowContext(ctx, `
		SELECT c.generation_id FROM person_fact_claims c
		WHERE c.person_id = ? AND c.target_kind = ? AND c.target_key = ?
		ORDER BY c.generation_id DESC, c.id DESC LIMIT 1`, personID, kind, key,
	).Scan(&generationID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("load latest person fact target generation: %w", err)
	}
	return generationID, true, nil
}
