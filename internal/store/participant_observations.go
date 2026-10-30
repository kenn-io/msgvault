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
	SourceID             *int64             `json:"source_id,omitzero" nullable:"false"`
	AddressKind          ContactAddressKind `json:"address_kind"`
	ServiceSlug          *string            `json:"service_slug,omitzero" nullable:"false"`
	ScopeKind            *string            `json:"scope_kind,omitzero" nullable:"false"`
	ScopeValue           *string            `json:"scope_value,omitzero" nullable:"false"`
	ProviderUserID       *string            `json:"provider_user_id,omitzero" nullable:"false"`
	OriginalValue        string             `json:"original_value"`
	NormalizedValue      string             `json:"normalized_value"`
	Normalization        string             `json:"normalization"`
	NormalizationVersion int                `json:"normalization_version"`
	ObservedAt           *time.Time         `json:"observed_at,omitempty"`
}

type ParticipantContactObservationInput struct {
	ConversationID *int64
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
	Observation  *ParticipantContactObservation `json:"observation"`
	Created      bool                           `json:"created"`
	Conflicting  bool                           `json:"conflicting"`
	CandidateIDs []int64                        `json:"candidate_ids,omitempty"`
	// CandidateID is the first conflict candidate, retained for callers that
	// predate complete conflict-graph reporting.
	CandidateID *int64 `json:"candidate_id,omitzero" nullable:"false"`
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

// sameParticipantObservationBinding keeps exact chat-provider identity
// provenance separate when observations share a normalized address. Native
// sources retain the historical address-based merge behavior.
func sameParticipantObservationBinding(left, right string) string {
	const tieredSources = `'beeper','slack','slackdump','discord','teams'`
	return `(NOT EXISTS (
		SELECT 1 FROM sources binding_source
		WHERE binding_source.id IN (` + left + `.source_id, ` + right + `.source_id)
		  AND binding_source.source_type IN (` + tieredSources + `)
	) OR (
		(` + left + `.provider_user_id = ` + right + `.provider_user_id OR
		 (` + left + `.provider_user_id IS NULL AND ` + right + `.provider_user_id IS NULL))
		AND (` + left + `.source_ref = ` + right + `.source_ref OR
		 (` + left + `.source_ref IS NULL AND ` + right + `.source_ref IS NULL))
	))`
}

// rewriteObservationsForMergeTx preserves archive contact evidence when one
// participant is absorbed into another. Current rows with the same logical
// identity collapse to the survivor's row with their room-support edges. The
// provider binding stays attached to that evidence while eligibility follows
// the exact support rooms' current policies. Historical rows do not participate
// in the current-value identity and are always repointed.
func (s *Store) rewriteObservationsForMergeTx(
	ctx context.Context, tx *loggedTx, absorbedID, survivorID int64,
) error {
	absorbedCurrent := `absorbed.active_until IS NULL AND absorbed.superseded_at IS NULL`
	survivorCurrent := `survivor.active_until IS NULL AND survivor.superseded_at IS NULL`
	sameAddress := `
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
	matchingIdentity := sameAddress + `
		AND ` + sameParticipantObservationBinding("survivor", "absorbed")

	// Outside the chat sources that carry room tiers, a provider binding is not
	// part of an observation's identity, so the absorbed duplicate below is
	// closed. Carry its stable provider ID onto the survivor first, or the merge
	// would lose the binding and leave a resolved conflict behind.
	untieredPair := `NOT EXISTS (SELECT 1 FROM sources binding_source
		WHERE binding_source.id IN (survivor.source_id, absorbed.source_id)
		  AND binding_source.source_type IN ('beeper','slack','slackdump','discord','teams'))`
	if _, err := tx.ExecContext(ctx, `
		UPDATE participant_contact_observations AS survivor
		SET provider_user_id = (
			SELECT absorbed.provider_user_id
			FROM participant_contact_observations AS absorbed
			WHERE absorbed.participant_id = ?
			  AND `+absorbedCurrent+`
			  AND absorbed.provider_user_id IS NOT NULL
			  AND `+sameAddress+`
			  AND `+untieredPair+`
			ORDER BY absorbed.id
			LIMIT 1
		), updated_at = `+s.dialect.Now()+`
		WHERE survivor.participant_id = ?
		  AND `+survivorCurrent+`
		  AND survivor.provider_user_id IS NULL
		  AND EXISTS (
			SELECT 1 FROM participant_contact_observations AS absorbed
			WHERE absorbed.participant_id = ?
			  AND `+absorbedCurrent+`
			  AND absorbed.provider_user_id IS NOT NULL
			  AND `+sameAddress+`
			  AND `+untieredPair+`
		)`, absorbedID, survivorID, absorbedID); err != nil {
		return fmt.Errorf("merge participant observation provider IDs: %w", err)
	}

	// Provider bindings remain recorded even when their rooms are Cold. Live
	// eligibility is evaluated from each row's exact room support, so merging
	// rows must not erase a binding before support is copied or duplicate rows
	// are identified.
	if _, err := tx.ExecContext(ctx, `INSERT INTO participant_contact_observation_conversations(observation_id,conversation_id)
        SELECT survivor.id, support.conversation_id FROM participant_contact_observations absorbed
        JOIN participant_contact_observations survivor ON `+matchingIdentity+`
        JOIN participant_contact_observation_conversations support ON support.observation_id=absorbed.id
        WHERE absorbed.participant_id=? AND survivor.participant_id=? AND `+absorbedCurrent+` AND `+survivorCurrent+`
        ON CONFLICT(observation_id,conversation_id) DO NOTHING`, absorbedID, survivorID); err != nil {
		return fmt.Errorf("merge observation room support: %w", err)
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
			  AND `+sameParticipantObservationBinding("survivor", "participant_contact_observations")+`
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
	input.ScopeKind = trimmedOrNil(input.ScopeKind)
	input.ScopeValue = trimmedOrNil(input.ScopeValue)
	input.ProviderUserID = trimmedOrNil(input.ProviderUserID)
	service, hasService, err := s.resolveOptionalCommunicationServiceContext(ctx, input.ServiceSlug)
	if err != nil {
		return nil, err
	}
	if err := ValidateServiceScope(service, input.ScopeKind, input.ScopeValue); err != nil {
		return nil, err
	}
	normalization := fallbackContactNormalization(input.AddressKind)
	normalizationVersion := 1
	var normalized string
	if input.AddressKind == ContactAddressProviderIdentity {
		// Provider identities are opaque, exact evidence. They are not user
		// addresses, so a service's phone/email/username normalization must not
		// lowercase, strip, or reject the synthetic key.
		normalized = input.OriginalValue
	} else {
		var err error
		normalized, err = NormalizeServiceValue(service, input.AddressKind, input.OriginalValue)
		if err != nil {
			return nil, err
		}
	}
	var serviceID any
	if hasService {
		serviceID = service.ID
		if input.AddressKind != ContactAddressProviderIdentity {
			normalization = service.Normalization
			normalizationVersion = service.NormalizationVersion
		}
	}
	result := &RecordContactObservationResult{}
	recordTx := func(ctx context.Context, fn func(*loggedTx) error) error {
		if input.ConversationID != nil {
			return s.withPolicyAttributionTxContext(ctx, fn)
		}
		return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, fn)
	}
	err = recordTx(ctx, func(tx *loggedTx) error {
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
			input.ProviderUserID, input.Envelope.SourceRef,
		)
		providerContradicted := false
		observationEnriched := false
		skipConversationSupport := false
		observationBindingChanged := false
		if errors.Is(err, ErrProfileValueNotFound) && input.SourceID != nil {
			chatSource, sourceErr := tieredChatObservationSourceTx(ctx, tx, *input.SourceID)
			if sourceErr != nil {
				return sourceErr
			}
			if chatSource {
				// A chat observation's provider binding is supported only by rooms
				// where that exact binding was observed. Keep providerless address
				// support and Beeper account/user provenance on separate rows.
				candidate, candidateErr := findCurrentParticipantObservationBySourceRefTx(
					ctx, tx, participantID, input.SourceID, input.AddressKind,
					serviceID, input.ScopeKind, input.ScopeValue, normalized,
					input.Envelope.SourceRef,
				)
				if candidateErr == nil {
					switch {
					case input.ProviderUserID == nil && candidate.ProviderUserID != nil:
						// Record this address-only value independently. Attaching its
						// room to the bound row would let that room authorize the ID.
					case input.ProviderUserID != nil && candidate.ProviderUserID == nil:
						safeToBind, bindErr := observationSupportsOnlyConversationTx(
							ctx, tx, candidate.Envelope.ID, input.ConversationID,
						)
						if bindErr != nil {
							return bindErr
						}
						originFull, originErr := observationOriginFullTx(
							ctx, tx, input.SourceID, input.ConversationID,
						)
						if originErr != nil {
							return originErr
						}
						if safeToBind && originFull {
							if _, err := tx.ExecContext(ctx,
								`UPDATE participant_contact_observations
								 SET provider_user_id = ?, updated_at = `+s.dialect.Now()+`
								 WHERE id = ?`,
								stringValue(input.ProviderUserID), candidate.Envelope.ID,
							); err != nil {
								return fmt.Errorf("update observation provider user ID: %w", err)
							}
							observation, err = getParticipantObservationTx(
								ctx, tx, participantID, candidate.Envelope.ID,
							)
							if err != nil {
								return err
							}
							result.Observation = observation
							observationEnriched = true
							observationBindingChanged = true
						}
					case input.ProviderUserID != nil && candidate.ProviderUserID != nil &&
						*candidate.ProviderUserID != *input.ProviderUserID:
						originFull, originErr := observationOriginFullTx(
							ctx, tx, input.SourceID, input.ConversationID,
						)
						if originErr != nil {
							return originErr
						}
						if !originFull {
							result.Observation = candidate
							observationEnriched = true
							skipConversationSupport = true
						} else {
							if err := s.supersedeObservationRowTx(
								ctx, tx, candidate.Envelope.ID,
							); err != nil {
								return err
							}
							providerContradicted = true
						}
					}
				} else if !errors.Is(candidateErr, ErrProfileValueNotFound) {
					return candidateErr
				}
			}
		}
		if err == nil {
			sameProvider := observation.ProviderUserID != nil &&
				input.ProviderUserID != nil &&
				*observation.ProviderUserID == *input.ProviderUserID
			if input.ProviderUserID == nil || sameProvider {
				result.Observation = observation
				added, err := s.recordObservationConversationTx(ctx, tx, observation, input.ConversationID)
				if err != nil {
					return err
				}
				if !added && !observationBindingChanged {
					return nil
				}
				// A new room support edge must reach matching even for a deduplicated value.
				observationEnriched = true
			} else {
				originFull, err := observationOriginFullTx(
					ctx, tx, input.SourceID, input.ConversationID,
				)
				if err != nil {
					return err
				}
				if !originFull {
					// A lower-tier observation cannot replace the one current row
					// for this participant, source, and normalized value. Keep the
					// Full-supported binding and do not attach the Cold room to it.
					result.Observation = observation
					observationEnriched = true
					skipConversationSupport = true
				} else if observation.ProviderUserID == nil {
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
					result.Observation = observation
					observationEnriched = true
					observationBindingChanged = true
				} else {
					// A Full observation with a different non-null provider ID
					// contradicts the current row. Close it and record the new
					// binding as a fresh observation so both facts survive in history.
					if err := s.supersedeObservationRowTx(
						ctx, tx, observation.Envelope.ID,
					); err != nil {
						return err
					}
					providerContradicted = true
				}
			}
		} else if !errors.Is(err, ErrProfileValueNotFound) {
			return err
		}
		if !observationEnriched {
			env, err := resolveProfileEnvelopeForOwnerTx(
				ctx, tx, "participant_contact_observations", "participant_id", "address_kind",
				participantID, input.AddressKind, input.Envelope,
			)
			if err != nil {
				return err
			}
			args := []any{
				participantID, int64Value(input.SourceID), input.AddressKind, serviceID,
				stringValue(input.ScopeKind), stringValue(input.ScopeValue),
				stringValue(input.ProviderUserID), input.OriginalValue, normalized,
				normalization, normalizationVersion, timeValue(input.ObservedAt),
			}
			args = append(args, profileEnvelopeArgs(env)...)
			var id int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO participant_contact_observations (
				participant_id, source_id, address_kind, service_id, scope_kind,
				scope_value, provider_user_id, original_value, normalized_value,
				normalization, normalization_version, observed_at, `+
				profileEnvelopeWriteColumns+`, created_at, updated_at
			) VALUES (
				?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
				?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
				`+s.dialect.Now()+`, `+s.dialect.Now()+`
			) RETURNING id`, args...).Scan(&id); err != nil {
				return fmt.Errorf("record participant contact observation: %w", err)
			}
			result.Observation, err = getParticipantObservationTx(ctx, tx, participantID, id)
			if err != nil {
				return err
			}
			result.Created = true
			if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
				return err
			}
			if providerContradicted {
				// Conflicts generated against the superseded provider binding may
				// no longer be supported by any current observation pair.
				if err := s.reconcileCurrentObservationIdentityMatchesTxContext(
					ctx, tx,
				); err != nil {
					return err
				}
			}
		}

		if !skipConversationSupport {
			if _, err := s.recordObservationConversationTx(ctx, tx, result.Observation, input.ConversationID); err != nil {
				return err
			}
		}
		if observationBindingChanged {
			if err := s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
				return err
			}
			if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
				return err
			}
		}
		eligible, err := fullObservationTx(ctx, tx, result.Observation.Envelope.ID)
		if err != nil || !eligible {
			return err
		}
		if result.Created {
			if err := s.invalidateContextualParticipantMessagesTx(ctx, tx, []int64{participantID}); err != nil {
				return fmt.Errorf("invalidate contextual messages for new participant observation: %w", err)
			}
		}

		result.CandidateIDs, err = s.recordObservationConflictsTx(ctx, tx, result.Observation, serviceID)
		if err != nil {
			return err
		}
		if len(result.CandidateIDs) > 0 {
			result.CandidateID = &result.CandidateIDs[0]
			result.Conflicting = true
		}
		return nil
	})
	return result, err
}

// recordProviderEmailObservationTx preserves provider email support in the
// exact conversation that admitted the metadata. The caller holds the
// conversation-policy and attribution locks and has already checked that the
// room is Full.
func (s *Store) recordProviderEmailObservationTx(
	ctx context.Context, tx *loggedTx, participantID, sourceID, roomID int64, email string,
) error {
	normalized, err := NormalizeServiceValue(nil, ContactAddressEmail, email)
	if err != nil {
		return err
	}
	if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "source-contact-observation", sourceID); err != nil {
		return err
	}
	if err := s.lockParticipantObservationOwnerTxContext(ctx, tx, participantID); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM participants WHERE id = ?`, participantID).Scan(&exists); err != nil {
		return fmt.Errorf("check participant: %w", err)
	}
	if exists == 0 {
		return ErrParticipantNotFound
	}
	if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "participant-contact-observation",
		participantID, sourceID, ContactAddressEmail, nil, nil, nil, normalized); err != nil {
		return err
	}
	observation, err := findParticipantObservationTx(
		ctx, tx, participantID, &sourceID, ContactAddressEmail, nil, nil, nil,
		normalized, nil, nil,
	)
	if errors.Is(err, ErrProfileValueNotFound) {
		envelope, envelopeErr := resolveProfileEnvelopeForOwnerTx(
			ctx, tx, "participant_contact_observations", "participant_id", "address_kind",
			participantID, ContactAddressEmail,
			ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
		)
		if envelopeErr != nil {
			return envelopeErr
		}
		args := []any{
			participantID, sourceID, ContactAddressEmail, nil, nil, nil, nil,
			email, normalized, fallbackContactNormalization(ContactAddressEmail), 1,
			timeValue(nil),
		}
		args = append(args, profileEnvelopeArgs(envelope)...)
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO participant_contact_observations (
			participant_id, source_id, address_kind, service_id, scope_kind,
			scope_value, provider_user_id, original_value, normalized_value,
			normalization, normalization_version, observed_at, `+
			profileEnvelopeWriteColumns+`, created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			`+s.dialect.Now()+`, `+s.dialect.Now()+`
		) RETURNING id`, args...).Scan(&id); err != nil {
			return fmt.Errorf("record provider email observation: %w", err)
		}
		observation, err = getParticipantObservationTx(ctx, tx, participantID, id)
		if err != nil {
			return err
		}
		if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	room := roomID
	if _, err := s.recordObservationConversationTx(ctx, tx, observation, &room); err != nil {
		return err
	}
	eligible, err := fullObservationTx(ctx, tx, observation.Envelope.ID)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrConversationPolicyStale
	}
	_, err = s.recordObservationConflictsTx(ctx, tx, observation, nil)
	return err
}

// recordConversationPolicyParticipantObservationTx records exact provider
// metadata after the caller has admitted the participant in this Full room.
// The support edge keeps names and identifiers out of derived views once the
// room becomes Cold.
func (s *Store) recordConversationPolicyParticipantObservationTx(
	ctx context.Context,
	tx *loggedTx,
	participantID, sourceID, roomID int64,
	addressKind ContactAddressKind,
	serviceSlug, scopeKind, scopeValue, providerUserID *string,
	value string,
) error {
	if addressKind != ContactAddressDisplayName && addressKind != ContactAddressProviderIdentity && addressKind != ContactAddressPhone {
		return ErrInvalidContactAddressKind
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ErrObservationValueMissing
	}
	scopeKind = trimmedOrNil(scopeKind)
	scopeValue = trimmedOrNil(scopeValue)
	providerUserID = trimmedOrNil(providerUserID)
	service, hasService, err := resolveCommunicationServiceTx(ctx, tx, serviceSlug)
	if err != nil {
		return err
	}
	if err := ValidateServiceScope(service, scopeKind, scopeValue); err != nil {
		return err
	}
	var normalized string
	if addressKind == ContactAddressProviderIdentity {
		normalized = value
	} else {
		normalized, err = NormalizeServiceValue(service, addressKind, value)
		if err != nil {
			return err
		}
	}
	var serviceID any
	normalization := fallbackContactNormalization(addressKind)
	version := 1
	if hasService {
		serviceID = service.ID
		if addressKind != ContactAddressProviderIdentity {
			normalization = service.Normalization
			version = service.NormalizationVersion
		}
	}
	if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "source-contact-observation", sourceID); err != nil {
		return err
	}
	if err := s.lockParticipantObservationOwnerTxContext(ctx, tx, participantID); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM participants WHERE id=?`, participantID).Scan(&exists); err != nil {
		return fmt.Errorf("check participant: %w", err)
	}
	if exists == 0 {
		return ErrParticipantNotFound
	}
	if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "participant-contact-observation",
		participantID, sourceID, addressKind, serviceID, stringValue(scopeKind),
		stringValue(scopeValue), normalized); err != nil {
		return err
	}
	observation, err := findParticipantObservationTx(
		ctx, tx, participantID, &sourceID, addressKind, serviceID,
		scopeKind, scopeValue, normalized, providerUserID, nil,
	)
	if errors.Is(err, ErrProfileValueNotFound) {
		envelope, envelopeErr := resolveProfileEnvelopeForOwnerTx(
			ctx, tx, "participant_contact_observations", "participant_id", "address_kind",
			participantID, addressKind,
			ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
		)
		if envelopeErr != nil {
			return envelopeErr
		}
		args := []any{
			participantID, sourceID, addressKind, serviceID,
			stringValue(scopeKind), stringValue(scopeValue), stringValue(providerUserID),
			value, normalized, normalization, version, timeValue(nil),
		}
		args = append(args, profileEnvelopeArgs(envelope)...)
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO participant_contact_observations (
			participant_id, source_id, address_kind, service_id, scope_kind,
			scope_value, provider_user_id, original_value, normalized_value,
			normalization, normalization_version, observed_at, `+
			profileEnvelopeWriteColumns+`, created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			`+s.dialect.Now()+`, `+s.dialect.Now()+`
		) RETURNING id`, args...).Scan(&id); err != nil {
			return fmt.Errorf("record participant policy observation: %w", err)
		}
		observation, err = getParticipantObservationTx(ctx, tx, participantID, id)
		if err != nil {
			return err
		}
		if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if providerUserID != nil {
		if observation.ProviderUserID != nil && *observation.ProviderUserID != *providerUserID {
			return fmt.Errorf("provider identity observation conflicts for participant %d", participantID)
		}
		if observation.ProviderUserID == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations
				SET provider_user_id=?, updated_at=`+s.dialect.Now()+` WHERE id=?`,
				*providerUserID, observation.Envelope.ID); err != nil {
				return fmt.Errorf("update provider identity observation: %w", err)
			}
			observation, err = getParticipantObservationTx(ctx, tx, participantID, observation.Envelope.ID)
			if err != nil {
				return err
			}
			if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
				return err
			}
		}
	}
	room := roomID
	if _, err := s.recordObservationConversationTx(ctx, tx, observation, &room); err != nil {
		return err
	}
	eligible, err := fullObservationTx(ctx, tx, observation.Envelope.ID)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrConversationPolicyStale
	}
	if addressKind != ContactAddressDisplayName {
		_, err = s.recordObservationConflictsTx(ctx, tx, observation, serviceID)
	}
	return err
}

func (s *Store) recordObservationConflictsTx(ctx context.Context, tx *loggedTx, observation *ParticipantContactObservation, serviceID any) ([]int64, error) {
	if observation.AddressKind == ContactAddressDisplayName {
		return nil, nil
	}
	var ids []int64
	otherParticipantIDs, err := findConflictingObservationParticipantIDsTx(
		ctx, tx, observation.ParticipantID, observation.AddressKind, serviceID,
		observation.ScopeKind, observation.ScopeValue, observation.NormalizedValue, observation.ProviderUserID,
	)
	if err != nil {
		return nil, err
	}
	if len(otherParticipantIDs) == 0 {
		return nil, nil
	}
	basis := identityMatchBasisForAddressKind(observation.AddressKind)
	for _, otherParticipantID := range otherParticipantIDs {
		leftKind, leftID, rightKind, rightID, err := canonicalMatchEndpoints(
			IdentityMatchParticipant, observation.ParticipantID,
			IdentityMatchParticipant, otherParticipantID,
		)
		if err != nil {
			return nil, err
		}
		candidateInput := IdentityMatchCandidateInput{
			LeftKind: leftKind, LeftID: leftID, RightKind: rightKind, RightID: rightID,
			Basis: basis, ServiceSlug: observation.ServiceSlug, ScopeKind: observation.ScopeKind,
			ScopeValue: observation.ScopeValue, NormalizedValue: &observation.NormalizedValue,
			State: IdentityMatchStateConflict, Source: ProvenanceArchiveObservation,
			SourceRef: observation.Envelope.SourceRef, SourceID: observation.SourceID,
		}
		candidate, _, err := s.upsertIdentityMatchCandidateTx(
			ctx, tx, candidateInput, leftKind, leftID, rightKind, rightID, serviceID, true,
		)
		if err != nil {
			return nil, err
		}
		ids = append(ids, candidate.ID)
	}
	return ids, nil
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
	query.ScopeKind = trimmedOrNil(query.ScopeKind)
	query.ScopeValue = trimmedOrNil(query.ScopeValue)
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
		return s.supersedeParticipantObservationTxContext(
			ctx, tx, participantID, observationID, activeUntil,
		)
	})
}

// SupersedeParticipantObservationFromConversationContext applies automatic
// rename supersession only while its originating room remains Full.
func (s *Store) SupersedeParticipantObservationFromConversationContext(
	ctx context.Context, participantID, observationID, conversationID int64,
) error {
	return s.withConversationPolicyIdentityTx(ctx, func(tx *loggedTx) error {
		var eligible bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM participant_contact_observations o
			JOIN conversations c ON c.id=? AND c.source_id=o.source_id
			WHERE o.id=? AND o.participant_id=?
			  AND o.active_until IS NULL AND o.superseded_at IS NULL
			  AND `+FullConversationWhere("c")+`
		)`, conversationID, observationID, participantID).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		return s.supersedeParticipantObservationTxContext(
			ctx, tx, participantID, observationID, nil,
		)
	})
}

func (s *Store) supersedeParticipantObservationTxContext(
	ctx context.Context, tx *loggedTx, participantID, observationID int64, activeUntil *time.Time,
) error {
	if err := s.lockParticipantObservationOwnerTxContext(ctx, tx, participantID); err != nil {
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
	if err := s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
		return err
	}
	return s.bumpParticipantIdentifierRevision(ctx, tx)
}

// reconcileCurrentObservationIdentityMatchesTxContext applies every cleanup
// rule whose truth depends on the current participant observations. Generated
// conflicts are demoted first so a promoted stable-ID or cross-scope candidate
// can be evaluated in its restored state by the general match cleanup.
func (s *Store) reconcileCurrentObservationIdentityMatchesTxContext(
	ctx context.Context, tx *loggedTx,
) error {
	if err := s.deleteUnsupportedObservationIdentityConflictsContext(ctx, tx); err != nil {
		return err
	}
	return s.recomputeUnsupportedCurrentObservationIdentityMatchesTxContext(ctx, tx)
}

// deleteUnsupportedObservationIdentityConflictsContext removes generated
// conflicts and returns promoted candidates to their pre-conflict state after
// the observations that support them change. A generated conflict with an
// independent Contacts claim becomes an ordinary candidate instead of deleting
// that claim or retaining its generated assessment. A conflict remains
// reviewable while a matching current observation pair exists whose stable
// provider IDs are absent or different.
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
			  AND (c.observation_conflict_origin = 'promoted'
			       OR (c.observation_conflict_origin = 'generated'
			           AND c.decided_by IS NULL
			           AND `+independentContactMatchSupportSQL("")+`))
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
		SET state = CASE WHEN observation_conflict_origin = 'generated' THEN 'candidate'
		                 ELSE COALESCE(pre_conflict_state, 'candidate') END,
		    confidence = CASE WHEN observation_conflict_origin = 'generated' THEN NULL ELSE confidence END,
		    notes = CASE WHEN observation_conflict_origin = 'generated' THEN NULL ELSE notes END,
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
	return scanParticipantContactObservations(rows)
}

// scanParticipantContactObservations drains participantObservationSelect rows,
// closing them before returning.
func scanParticipantContactObservations(
	rows *loggedRows,
) ([]ParticipantContactObservation, error) {
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
	providerUserID, sourceRef *string,
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
		  AND o.active_until IS NULL AND o.superseded_at IS NULL
		  AND (NOT EXISTS (
			SELECT 1 FROM sources identity_source WHERE identity_source.id = o.source_id
			  AND identity_source.source_type IN ('beeper','slack','slackdump','discord','teams')
		  ) OR (
			(o.provider_user_id = ? OR (o.provider_user_id IS NULL AND CAST(? AS TEXT) IS NULL))
			AND (o.source_ref = ? OR (o.source_ref IS NULL AND CAST(? AS TEXT) IS NULL))
		  ))
		ORDER BY o.id LIMIT 1`,
		participantID, int64Value(sourceID), int64Value(sourceID), addressKind,
		serviceID, serviceID,
		stringValue(scopeKind), stringValue(scopeKind),
		stringValue(scopeValue), stringValue(scopeValue), normalized,
		stringValue(providerUserID), stringValue(providerUserID),
		stringValue(sourceRef), stringValue(sourceRef),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return observation, err
}

func findCurrentParticipantObservationBySourceRefTx(
	ctx context.Context,
	tx *loggedTx,
	participantID int64,
	sourceID *int64,
	addressKind ContactAddressKind,
	serviceID any,
	scopeKind, scopeValue *string,
	normalized string,
	sourceRef *string,
) (*ParticipantContactObservation, error) {
	observation, err := scanParticipantObservation(tx.QueryRowContext(ctx,
		participantObservationSelect+`
		WHERE o.participant_id = ?
		  AND o.source_id = ? AND o.address_kind = ?
		  AND (o.service_id = ? OR (o.service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (o.scope_kind = ? OR (o.scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (o.scope_value = ? OR (o.scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND o.normalized_value = ?
		  AND (o.source_ref = ? OR (o.source_ref IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND o.active_until IS NULL AND o.superseded_at IS NULL
		ORDER BY o.id LIMIT 1`,
		participantID, *sourceID, addressKind, serviceID, serviceID,
		stringValue(scopeKind), stringValue(scopeKind),
		stringValue(scopeValue), stringValue(scopeValue), normalized,
		stringValue(sourceRef), stringValue(sourceRef),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return observation, err
}

func tieredChatObservationSourceTx(ctx context.Context, tx *loggedTx, sourceID int64) (bool, error) {
	var isChat bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sources WHERE id = ?
		  AND source_type IN ('beeper','slack','slackdump','discord','teams')
	)`, sourceID).Scan(&isChat)
	return isChat, err
}

func observationSupportsOnlyConversationTx(
	ctx context.Context, tx *loggedTx, observationID int64, conversationID *int64,
) (bool, error) {
	var otherSupportCount int
	if conversationID == nil {
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM participant_contact_observation_conversations WHERE observation_id = ?`,
			observationID).Scan(&otherSupportCount)
		return otherSupportCount == 0, err
	}
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM participant_contact_observation_conversations
		WHERE observation_id = ? AND conversation_id <> ?`,
		observationID, *conversationID).Scan(&otherSupportCount)
	return otherSupportCount == 0, err
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
		FROM participant_contact_observations o
		WHERE `+FullObservationWhere("o")+` AND participant_id != ? AND address_kind = ?
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
	case ContactAddressProviderIdentity:
		return IdentityMatchStableProviderID
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
	o.source_ref, o.source_resource_uid, o.confidence, o.active_from, o.active_until,
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
