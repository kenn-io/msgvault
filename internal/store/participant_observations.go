package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ParticipantContactObservation struct {
	Envelope             ValueEnvelope      `json:"envelope"`
	ParticipantID        int64              `json:"participant_id"`
	SourceID             *int64             `json:"source_id,omitempty"`
	AddressKind          ContactAddressKind `json:"address_kind"`
	ServiceSlug          *string            `json:"service_slug,omitempty"`
	ScopeKind            *string            `json:"scope_kind,omitempty"`
	ScopeValue           *string            `json:"scope_value,omitempty"`
	ProviderUserID       *string            `json:"provider_user_id,omitempty"`
	OriginalValue        string             `json:"original_value"`
	NormalizedValue      string             `json:"normalized_value"`
	Normalization        string             `json:"normalization"`
	NormalizationVersion int                `json:"normalization_version"`
	ObservedAt           *time.Time         `json:"observed_at,omitempty"`
}

type ParticipantContactObservationInput struct {
	SourceID       *int64
	AddressKind    ContactAddressKind
	ServiceSlug    *string
	ScopeKind      *string
	ScopeValue     *string
	ProviderUserID *string
	OriginalValue  string
	ObservedAt     *time.Time
	Envelope       ValueEnvelopeInput
}

type RecordContactObservationResult struct {
	Observation *ParticipantContactObservation `json:"observation"`
	Created     bool                           `json:"created"`
	Conflicting bool                           `json:"conflicting"`
	CandidateID *int64                         `json:"candidate_id,omitempty"`
}

var ErrObservationValueMissing = errors.New("participant contact observation requires a non-empty value")

func (s *Store) lockParticipantObservationOwnerTxContext(
	ctx context.Context, tx *loggedTx, participantID int64,
) error {
	return s.lockProfileIdentityKeyTxContext(
		ctx, tx, "participant-contact-observation-owner", participantID,
	)
}

func (s *Store) lockParticipantObservationMergeTx(
	ctx context.Context, tx *loggedTx, leftID, rightID int64,
) error {
	if leftID > rightID {
		leftID, rightID = rightID, leftID
	}
	if err := s.lockParticipantObservationOwnerTxContext(ctx, tx, leftID); err != nil {
		return err
	}
	return s.lockParticipantObservationOwnerTxContext(ctx, tx, rightID)
}

// rewriteObservationsForMergeTx preserves archive contact evidence when one
// participant is absorbed into another. Current rows with the same logical
// identity collapse to the survivor's row; a stable provider ID fills an empty
// survivor value before the absorbed duplicate is closed. Historical rows do
// not participate in the current-value identity and are always repointed.
func (s *Store) rewriteObservationsForMergeTx(
	ctx context.Context, tx *loggedTx, absorbedID, survivorID int64,
) error {
	absorbedCurrent := `absorbed.active_until IS NULL AND absorbed.superseded_at IS NULL`
	survivorCurrent := `survivor.active_until IS NULL AND survivor.superseded_at IS NULL`
	matchingIdentity := `
		(survivor.source_id = absorbed.source_id OR
			(survivor.source_id IS NULL AND absorbed.source_id IS NULL))
		AND survivor.address_kind = absorbed.address_kind
		AND (survivor.service_id = absorbed.service_id OR
			(survivor.service_id IS NULL AND absorbed.service_id IS NULL))
		AND (survivor.scope_kind = absorbed.scope_kind OR
			(survivor.scope_kind IS NULL AND absorbed.scope_kind IS NULL))
		AND (survivor.scope_value = absorbed.scope_value OR
			(survivor.scope_value IS NULL AND absorbed.scope_value IS NULL))
		AND survivor.normalized_value = absorbed.normalized_value`

	if _, err := tx.ExecContext(ctx, `
		UPDATE participant_contact_observations AS survivor
		SET provider_user_id = COALESCE(survivor.provider_user_id, (
			SELECT absorbed.provider_user_id
			FROM participant_contact_observations AS absorbed
			WHERE absorbed.participant_id = ?
			  AND `+absorbedCurrent+`
			  AND absorbed.provider_user_id IS NOT NULL
			  AND `+matchingIdentity+`
			ORDER BY absorbed.id
			LIMIT 1
		)), updated_at = `+s.dialect.Now()+`
		WHERE survivor.participant_id = ?
		  AND `+survivorCurrent+`
		  AND EXISTS (
			SELECT 1 FROM participant_contact_observations AS absorbed
			WHERE absorbed.participant_id = ?
			  AND `+absorbedCurrent+`
			  AND `+matchingIdentity+`
		)`, absorbedID, survivorID, absorbedID); err != nil {
		return fmt.Errorf("merge participant observation provider IDs: %w", err)
	}

	now := s.dialect.Now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE participant_contact_observations
		SET active_until = CASE WHEN active_from > `+now+`
				THEN active_from ELSE `+now+` END,
			superseded_at = `+now+`, updated_at = `+now+`
		WHERE participant_id = ?
		  AND active_until IS NULL AND superseded_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM participant_contact_observations AS survivor
			WHERE survivor.participant_id = ?
			  AND survivor.active_until IS NULL AND survivor.superseded_at IS NULL
			  AND (survivor.source_id = participant_contact_observations.source_id OR
				(survivor.source_id IS NULL AND participant_contact_observations.source_id IS NULL))
			  AND survivor.address_kind = participant_contact_observations.address_kind
			  AND (survivor.service_id = participant_contact_observations.service_id OR
				(survivor.service_id IS NULL AND participant_contact_observations.service_id IS NULL))
			  AND (survivor.scope_kind = participant_contact_observations.scope_kind OR
				(survivor.scope_kind IS NULL AND participant_contact_observations.scope_kind IS NULL))
			  AND (survivor.scope_value = participant_contact_observations.scope_value OR
				(survivor.scope_value IS NULL AND participant_contact_observations.scope_value IS NULL))
			  AND survivor.normalized_value = participant_contact_observations.normalized_value
		)`, absorbedID, survivorID); err != nil {
		return fmt.Errorf("close duplicate merged participant observations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations
		SET participant_id = ? WHERE participant_id = ?`, survivorID, absorbedID); err != nil {
		return fmt.Errorf("repoint merged participant observations: %w", err)
	}
	return nil
}

func (s *Store) RecordContactObservationContext(
	ctx context.Context, participantID int64, input ParticipantContactObservationInput,
) (*RecordContactObservationResult, error) {
	if !input.AddressKind.Valid() {
		return nil, ErrInvalidContactAddressKind
	}
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.OriginalValue) == "" {
		return nil, ErrObservationValueMissing
	}
	input.ScopeKind = normalizeScopeInput(input.ScopeKind)
	input.ScopeValue = normalizeScopeInput(input.ScopeValue)
	service, hasService, err := s.resolveOptionalCommunicationServiceContext(ctx, input.ServiceSlug)
	if err != nil {
		return nil, err
	}
	if err := ValidateServiceScope(service, input.ScopeKind, input.ScopeValue); err != nil {
		return nil, err
	}
	normalized, err := NormalizeServiceValue(service, input.AddressKind, input.OriginalValue)
	if err != nil {
		return nil, err
	}
	normalization := fallbackContactNormalization(input.AddressKind)
	normalizationVersion := 1
	var serviceID any
	if hasService {
		serviceID = service.ID
		normalization = service.Normalization
		normalizationVersion = service.NormalizationVersion
	}
	result := &RecordContactObservationResult{}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		// Observation writes can create participant-level identity conflicts.
		// Use the same outer lock as conflict cleanup, source removal, and
		// participant merge so none of them can act on a stale observation set.
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		// Normal source removal takes this source-scoped lock before it
		// recomputes generated conflicts and deletes the source. An importer for
		// that source must finish its observation/candidate transaction before
		// the cleanup snapshot, or start after the source has gone and fail its
		// foreign key check. Serialized removal gets the same exclusion from its
		// table locks.
		if input.SourceID != nil {
			if err := s.lockProfileIdentityKeyTxContext(
				ctx, tx, "source-contact-observation", *input.SourceID,
			); err != nil {
				return err
			}
		}
		// Participant merge takes these owner locks in ID order before it
		// rewrites observations. Recording takes the one owner lock before its
		// narrower identity-key lock, so a merge cannot delete the participant
		// between validation and insert.
		if err := s.lockParticipantObservationOwnerTxContext(
			ctx, tx, participantID,
		); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM participants WHERE id = ?`, participantID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check participant: %w", err)
		}
		if exists == 0 {
			return ErrParticipantNotFound
		}
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "participant-contact-observation",
			participantID, int64Value(input.SourceID), input.AddressKind, serviceID,
			stringValue(input.ScopeKind), stringValue(input.ScopeValue), normalized,
		); err != nil {
			return err
		}
		observation, err := findParticipantObservationTx(
			ctx, tx, participantID, input.SourceID, input.AddressKind, serviceID,
			input.ScopeKind, input.ScopeValue, normalized,
		)
		providerContradicted := false
		if err == nil {
			sameProvider := observation.ProviderUserID != nil &&
				input.ProviderUserID != nil &&
				*observation.ProviderUserID == *input.ProviderUserID
			if input.ProviderUserID == nil || sameProvider {
				result.Observation = observation
				return nil
			}
			if observation.ProviderUserID == nil {
				if _, err := tx.ExecContext(ctx,
					`UPDATE participant_contact_observations
					 SET provider_user_id = ?, updated_at = `+s.dialect.Now()+`
					 WHERE id = ?`,
					stringValue(input.ProviderUserID), observation.Envelope.ID,
				); err != nil {
					return fmt.Errorf("update observation provider user ID: %w", err)
				}
				observation, err = getParticipantObservationTx(
					ctx, tx, participantID, observation.Envelope.ID,
				)
				if err != nil {
					return err
				}
				if err := s.deleteUnsupportedObservationIdentityConflictsContext(
					ctx, tx,
				); err != nil {
					return err
				}
				result.Observation = observation
				return nil
			}
			// A different non-null provider ID contradicts the current row.
			// Close it and record the new binding as a fresh observation so
			// both facts survive in history.
			if err := s.supersedeObservationRowTx(
				ctx, tx, observation.Envelope.ID,
			); err != nil {
				return err
			}
			providerContradicted = true
		} else if !errors.Is(err, ErrProfileValueNotFound) {
			return err
		}
		args := []any{
			participantID, int64Value(input.SourceID), input.AddressKind, serviceID,
			stringValue(input.ScopeKind), stringValue(input.ScopeValue),
			stringValue(input.ProviderUserID), input.OriginalValue, normalized,
			normalization, normalizationVersion, timeValue(input.ObservedAt),
		}
		args = append(args, profileEnvelopeArgs(input.Envelope.valueEnvelope(0))...)
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO participant_contact_observations (
			participant_id, source_id, address_kind, service_id, scope_kind,
			scope_value, provider_user_id, original_value, normalized_value,
			normalization, normalization_version, observed_at, `+
			profileEnvelopeWriteColumns+`, created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			`+s.dialect.Now()+`, `+s.dialect.Now()+`
		) RETURNING id`, args...).Scan(&id); err != nil {
			return fmt.Errorf("record participant contact observation: %w", err)
		}
		result.Observation, err = getParticipantObservationTx(ctx, tx, participantID, id)
		if err != nil {
			return err
		}
		result.Created = true
		if err := s.bumpParticipantIdentifierRevision(tx); err != nil {
			return err
		}
		if providerContradicted {
			// Conflicts generated against the superseded provider binding may
			// no longer be supported by any current observation pair.
			if err := s.deleteUnsupportedObservationIdentityConflictsContext(
				ctx, tx,
			); err != nil {
				return err
			}
		}

		otherParticipantIDs, err := findConflictingObservationParticipantIDsTx(
			ctx, tx, participantID, input.AddressKind, serviceID,
			input.ScopeKind, input.ScopeValue, normalized, input.ProviderUserID,
		)
		if err != nil {
			return err
		}
		if len(otherParticipantIDs) == 0 {
			return nil
		}
		basis := identityMatchBasisForAddressKind(input.AddressKind)
		for _, otherParticipantID := range otherParticipantIDs {
			leftKind, leftID, rightKind, rightID, err := canonicalMatchEndpoints(
				IdentityMatchParticipant, participantID,
				IdentityMatchParticipant, otherParticipantID,
			)
			if err != nil {
				return err
			}
			candidateInput := IdentityMatchCandidateInput{
				LeftKind: leftKind, LeftID: leftID, RightKind: rightKind, RightID: rightID,
				Basis: basis, ServiceSlug: input.ServiceSlug, ScopeKind: input.ScopeKind,
				ScopeValue: input.ScopeValue, NormalizedValue: &normalized,
				State: IdentityMatchStateConflict, Source: ProvenanceArchiveObservation,
			}
			candidate, _, err := s.upsertIdentityMatchCandidateTx(
				ctx, tx, candidateInput, leftKind, leftID, rightKind, rightID, serviceID, true,
			)
			if err != nil {
				return err
			}
			if result.CandidateID == nil {
				result.CandidateID = &candidate.ID
			}
		}
		result.Conflicting = true
		return nil
	})
	return result, err
}

// supersedeObservationRowTx closes one current observation row in place,
// mirroring the close semantics used by merge deduplication.
func (s *Store) supersedeObservationRowTx(
	ctx context.Context, tx *loggedTx, observationID int64,
) error {
	now := s.dialect.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations
		SET active_until = CASE WHEN active_from > `+now+`
				THEN active_from ELSE `+now+` END,
			superseded_at = `+now+`, updated_at = `+now+`
		WHERE id = ?`, observationID); err != nil {
		return fmt.Errorf("supersede contradicted participant observation: %w", err)
	}
	return nil
}

func (s *Store) ListParticipantObservationsContext(
	ctx context.Context, participantID int64, currentOnly bool,
) ([]ParticipantContactObservation, error) {
	query := participantObservationSelect + ` WHERE o.participant_id = ?`
	if currentOnly {
		query += ` AND o.active_until IS NULL AND o.superseded_at IS NULL`
	}
	query += ` ORDER BY o.address_kind, o.ordinal, o.id`
	return s.queryParticipantObservationsContext(ctx, query, participantID)
}

func (s *Store) listObservationsForPersonTx(
	ctx context.Context, tx *loggedTx, personID int64,
) ([]ParticipantContactObservation, error) {
	query := participantObservationSelect + `
		WHERE EXISTS (
			SELECT 1 FROM person_participants pp
			WHERE pp.participant_id = o.participant_id AND pp.person_id = ?
		)
		ORDER BY o.participant_id, o.id`
	return queryProfileRowsTx(ctx, tx, query, scanParticipantObservation, personID)
}

func (s *Store) FindObservationsByAddressContext(
	ctx context.Context, query ContactPointQuery,
) ([]ParticipantContactObservation, error) {
	if !query.AddressKind.Valid() {
		return nil, ErrInvalidContactAddressKind
	}
	query.ScopeKind = normalizeScopeInput(query.ScopeKind)
	query.ScopeValue = normalizeScopeInput(query.ScopeValue)
	service, hasService, err := s.resolveOptionalCommunicationServiceContext(ctx, query.ServiceSlug)
	if err != nil {
		return nil, err
	}
	var serviceID any
	if hasService {
		serviceID = service.ID
	}
	return s.queryParticipantObservationsContext(ctx, participantObservationSelect+`
		WHERE o.address_kind = ?
		  AND (o.service_id = ? OR (o.service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (o.scope_kind = ? OR (o.scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (o.scope_value = ? OR (o.scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND o.normalized_value = ?
		  AND o.active_until IS NULL AND o.superseded_at IS NULL
		ORDER BY o.participant_id, o.id`,
		query.AddressKind, serviceID, serviceID,
		stringValue(query.ScopeKind), stringValue(query.ScopeKind),
		stringValue(query.ScopeValue), stringValue(query.ScopeValue),
		query.NormalizedValue,
	)
}

func (s *Store) SupersedeParticipantObservationContext(
	ctx context.Context, participantID, observationID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.lockParticipantObservationOwnerTxContext(
			ctx, tx, participantID,
		); err != nil {
			return err
		}
		if err := s.validateProfileValueCloseTimeTx(
			ctx, tx, "participant_contact_observations", "participant_id",
			participantID, observationID, activeUntil,
		); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations
			SET active_until = COALESCE(active_until, ?,
			        CASE WHEN active_from > `+s.dialect.Now()+`
			             THEN active_from ELSE `+s.dialect.Now()+` END),
			    superseded_at = `+s.dialect.Now()+`,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND participant_id = ?
			  AND superseded_at IS NULL`,
			timeValue(activeUntil), observationID, participantID,
		)
		if err != nil {
			return fmt.Errorf("supersede participant observation: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return ErrProfileValueNotFound
		}
		if err := s.deleteUnsupportedObservationIdentityConflictsContext(
			ctx, tx,
		); err != nil {
			return err
		}
		return s.bumpParticipantIdentifierRevision(tx)
	})
}

// deleteUnsupportedObservationIdentityConflictsContext removes generated
// conflicts and returns promoted candidates to their pre-conflict state after
// the observations that support them change. A conflict remains reviewable
// while a matching current observation pair exists whose stable provider IDs
// are absent or different.
func (s *Store) deleteUnsupportedObservationIdentityConflictsContext(
	ctx context.Context, execer contextQuerier,
) error {
	if _, err := execer.ExecContext(ctx, s.dialect.Rebind(`
		WITH stale_conflicts AS (
			SELECT c.id
			FROM identity_match_candidates c
			WHERE c.left_kind = 'participant'
			  AND c.right_kind = 'participant'
			  AND c.state = 'conflict'
			  AND c.observation_conflict_origin = 'promoted'
			  AND c.normalized_value IS NOT NULL
			  AND c.basis IN ('email', 'phone', 'service_scope_username')
			  AND NOT EXISTS (
				SELECT 1 FROM participant_contact_observations current_left
				WHERE current_left.participant_id = c.left_id
				  AND `+identityCandidateObservationMatchSQL("current_left")+`
				  AND EXISTS (
					SELECT 1 FROM participant_contact_observations current_right
					WHERE current_right.participant_id = c.right_id
					  AND `+identityCandidateObservationMatchSQL("current_right")+`
					  AND `+identityCandidateObservationPairSQL(
		"current_left", "current_right",
	)+`
				  )
			  )
		)
		UPDATE identity_match_candidates
		SET state = COALESCE(pre_conflict_state, 'candidate'),
		    observation_conflict_origin = NULL, pre_conflict_state = NULL,
		    updated_at = `+s.dialect.Now()+`
		WHERE id IN (SELECT id FROM stale_conflicts)`)); err != nil {
		return fmt.Errorf("demote unsupported observation conflicts: %w", err)
	}
	if _, err := execer.ExecContext(ctx, s.dialect.Rebind(`
		WITH stale_conflicts AS (
			SELECT c.id
			FROM identity_match_candidates c
			WHERE c.left_kind = 'participant'
			  AND c.right_kind = 'participant'
			  AND c.state = 'conflict'
			  AND c.observation_conflict_origin = 'generated'
			  AND c.normalized_value IS NOT NULL
			  AND c.basis IN ('email', 'phone', 'service_scope_username')
			  AND NOT EXISTS (
				SELECT 1 FROM participant_contact_observations current_left
				WHERE current_left.participant_id = c.left_id
				  AND `+identityCandidateObservationMatchSQL("current_left")+`
				  AND EXISTS (
					SELECT 1 FROM participant_contact_observations current_right
					WHERE current_right.participant_id = c.right_id
					  AND `+identityCandidateObservationMatchSQL("current_right")+`
					  AND `+identityCandidateObservationPairSQL(
		"current_left", "current_right",
	)+`
				  )
			  )
		)
		DELETE FROM identity_match_candidates
		WHERE id IN (SELECT id FROM stale_conflicts)`)); err != nil {
		return fmt.Errorf("delete unsupported observation conflicts: %w", err)
	}
	return nil
}

// identityCandidateObservationPairSQL matches a supporting observation pair:
// conflict generation only ever pairs observations of the same address kind,
// so a cross-kind pair (say username vs social) must not keep a conflict
// alive either.
func identityCandidateObservationPairSQL(leftAlias, rightAlias string) string {
	return leftAlias + `.address_kind = ` + rightAlias + `.address_kind
		AND (` + leftAlias + `.provider_user_id IS NULL
		OR ` + rightAlias + `.provider_user_id IS NULL
		OR ` + leftAlias + `.provider_user_id != ` + rightAlias + `.provider_user_id)`
}

func (s *Store) queryParticipantObservationsContext(
	ctx context.Context, query string, args ...any,
) ([]ParticipantContactObservation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query participant observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	observations := make([]ParticipantContactObservation, 0)
	for rows.Next() {
		observation, err := scanParticipantObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan participant observation: %w", err)
		}
		observations = append(observations, *observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query participant observations: %w", err)
	}
	return observations, nil
}

func findParticipantObservationTx(
	ctx context.Context,
	tx *loggedTx,
	participantID int64,
	sourceID *int64,
	addressKind ContactAddressKind,
	serviceID any,
	scopeKind, scopeValue *string,
	normalized string,
) (*ParticipantContactObservation, error) {
	observation, err := scanParticipantObservation(tx.QueryRowContext(ctx,
		participantObservationSelect+`
		WHERE o.participant_id = ?
		  AND (o.source_id = ? OR (o.source_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND o.address_kind = ?
		  AND (o.service_id = ? OR (o.service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (o.scope_kind = ? OR (o.scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (o.scope_value = ? OR (o.scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND o.normalized_value = ?
		  AND o.active_until IS NULL AND o.superseded_at IS NULL`,
		participantID, int64Value(sourceID), int64Value(sourceID), addressKind,
		serviceID, serviceID,
		stringValue(scopeKind), stringValue(scopeKind),
		stringValue(scopeValue), stringValue(scopeValue), normalized,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return observation, err
}

func findConflictingObservationParticipantIDsTx(
	ctx context.Context,
	tx *loggedTx,
	participantID int64,
	addressKind ContactAddressKind,
	serviceID any,
	scopeKind, scopeValue *string,
	normalized string,
	providerUserID *string,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT participant_id, provider_user_id
		FROM participant_contact_observations
		WHERE participant_id != ? AND address_kind = ?
		  AND (service_id = ? OR (service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (scope_kind = ? OR (scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (scope_value = ? OR (scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND normalized_value = ?
		  AND active_until IS NULL AND superseded_at IS NULL
		ORDER BY participant_id, id`,
		participantID, addressKind, serviceID, serviceID,
		stringValue(scopeKind), stringValue(scopeKind),
		stringValue(scopeValue), stringValue(scopeValue), normalized,
	)
	if err != nil {
		return nil, fmt.Errorf("find conflicting observation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	participantIDs := make([]int64, 0)
	seen := make(map[int64]struct{})
	for rows.Next() {
		var otherID int64
		var otherProvider sql.NullString
		if err := rows.Scan(&otherID, &otherProvider); err != nil {
			return nil, err
		}
		if providerUserID != nil && otherProvider.Valid && otherProvider.String == *providerUserID {
			continue
		}
		if _, ok := seen[otherID]; ok {
			continue
		}
		seen[otherID] = struct{}{}
		participantIDs = append(participantIDs, otherID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return participantIDs, nil
}

func identityMatchBasisForAddressKind(kind ContactAddressKind) IdentityMatchBasis {
	switch kind {
	case ContactAddressEmail:
		return IdentityMatchEmail
	case ContactAddressPhone:
		return IdentityMatchPhone
	default:
		return IdentityMatchServiceScopeUsername
	}
}

const participantObservationSelect = `SELECT
	o.id, o.participant_id, o.source_id, o.address_kind, cs.slug,
	o.scope_kind, o.scope_value, o.provider_user_id, o.original_value,
	o.normalized_value, o.normalization, o.normalization_version,
	o.observed_at,
	o.pref, o.ordinal, o.type_label, o.type_tokens, o.vcard_property,
	o.vcard_group, o.vcard_prop_id, o.vcard_pid, o.vcard_altid, o.source,
	o.source_ref, o.confidence, o.active_from, o.active_until,
	o.created_at, o.updated_at, o.superseded_at
	FROM participant_contact_observations o
	LEFT JOIN communication_services cs ON cs.id = o.service_id`

func getParticipantObservationTx(
	ctx context.Context, tx *loggedTx, participantID, id int64,
) (*ParticipantContactObservation, error) {
	observation, err := scanParticipantObservation(tx.QueryRowContext(ctx,
		participantObservationSelect+` WHERE o.participant_id = ? AND o.id = ?`,
		participantID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return observation, err
}

func scanParticipantObservation(row scanner) (*ParticipantContactObservation, error) {
	var observation ParticipantContactObservation
	var sourceID sql.NullInt64
	var serviceSlug, scopeKind, scopeValue, providerUserID sql.NullString
	var observedAt sql.NullTime
	var env profileEnvelopeScanValues
	dest := []any{
		&observation.Envelope.ID, &observation.ParticipantID, &sourceID,
		&observation.AddressKind, &serviceSlug, &scopeKind, &scopeValue,
		&providerUserID, &observation.OriginalValue, &observation.NormalizedValue,
		&observation.Normalization, &observation.NormalizationVersion, &observedAt,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	observation.SourceID = nullInt64Ptr(sourceID)
	observation.ServiceSlug = nullStringPtr(serviceSlug)
	observation.ScopeKind = nullStringPtr(scopeKind)
	observation.ScopeValue = nullStringPtr(scopeValue)
	observation.ProviderUserID = nullStringPtr(providerUserID)
	observation.ObservedAt = nullTimePtr(observedAt)
	if err := env.apply(&observation.Envelope); err != nil {
		return nil, err
	}
	return &observation, nil
}

func int64Value(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}
