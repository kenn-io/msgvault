package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// XAuthorInput is a normalized provider observation, with its original fetch
// time retained across local replay. No bare username creates an identity.
type XAuthorInput struct {
	SourceID    int64
	UserID      string
	Username    string
	DisplayName string
	Protected   bool
	FetchedAt   time.Time
}

// RecordXAuthorContext creates the numeric identity and its observations in a
// single attribution transaction. Writers reuse recordXAuthorTx for atomicity.
func (s *Store) RecordXAuthorContext(ctx context.Context, input XAuthorInput) (int64, error) {
	var id int64
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		var err error
		id, err = s.recordXAuthorTx(ctx, tx, input)
		return err
	})
	return id, err
}

func validXAuthorID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value[0] == '0' {
		return false
	}
	for _, b := range []byte(value) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// xAuthorUnknown and xAuthorProtected are content-free profile-evidence
// markers, preventing equal-time/stale pages from reopening protected labels.
const (
	xAuthorUnknown   = "x_author_unknown"
	xAuthorProtected = "x_author_protected"
)

func (s *Store) recordXAuthorTx(ctx context.Context, tx *loggedTx, input XAuthorInput) (int64, error) {
	if err := s.requireSyncSource(input.SourceID); err != nil {
		return 0, err
	}
	if input.SourceID <= 0 || !validXAuthorID(input.UserID) || input.FetchedAt.IsZero() {
		return 0, errors.New("invalid X author observation")
	}
	var sourceType, ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT source_type,identifier FROM sources WHERE id=?`, input.SourceID).Scan(&sourceType, &ownerID); err != nil {
		return 0, fmt.Errorf("read X author source: %w", err)
	}
	if sourceType != "x" || !validXAuthorID(ownerID) {
		return 0, errors.New("X author source does not match a numeric X account")
	}
	// Delay name backfill until the response's freshness has been checked.
	pid, err := s.ensureParticipantByIdentifierTx(ctx, tx, "x_user_id", input.UserID, "")
	if err != nil {
		return 0, err
	}
	anchor := beeperidentity.ProviderIdentityKey("x", "service", "", input.UserID)
	observedAt := input.FetchedAt.UTC()
	profileKnown := input.Username != "" || input.Protected
	envelope := ValueEnvelopeInput{Source: ProvenanceArchiveObservation, SourceRef: new(fmt.Sprintf("x:%d:author:%s", input.SourceID, input.UserID)), SourceResourceUID: new("x:author:" + input.UserID)}
	base := ParticipantContactObservationInput{SourceID: new(input.SourceID), ServiceSlug: new("x"), ProviderUserID: new(anchor), ObservedAt: &observedAt, Envelope: envelope}
	provider := base
	provider.AddressKind = ContactAddressProviderIdentity
	provider.OriginalValue = anchor
	if input.Protected {
		provider.Envelope.TypeTokens = []string{xAuthorProtected}
	} else if !profileKnown {
		provider.Envelope.TypeTokens = []string{xAuthorUnknown}
	}
	providerResult, err := s.recordContactObservationTx(ctx, tx, pid, provider)
	if err != nil {
		return 0, err
	}
	prior := providerResult.Observation
	observations, err := queryProfileRowsTx(ctx, tx, participantObservationSelect+` WHERE o.participant_id=? AND o.source_id=? AND o.address_kind='username' AND o.provider_user_id=? AND o.source='archive_observation' AND o.active_until IS NULL AND o.superseded_at IS NULL ORDER BY o.id`, scanParticipantObservation, pid, input.SourceID, anchor)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(prior.Envelope.TypeTokens, xAuthorUnknown) && prior.ObservedAt != nil {
		if observedAt.Before(*prior.ObservedAt) {
			return pid, nil
		}
		if observedAt.Equal(*prior.ObservedAt) && slices.Contains(prior.Envelope.TypeTokens, xAuthorProtected) && !input.Protected {
			return pid, nil
		}
	}
	for _, o := range observations {
		if o.ObservedAt != nil && observedAt.Before(*o.ObservedAt) {
			return pid, nil
		}
	}
	// Missing expansion is no newer profile fact. In particular it cannot clear
	// the last explicit protected observation, though the numeric anchor remains.
	if !profileKnown {
		return pid, nil
	}
	if prior.Envelope.Source == ProvenanceArchiveObservation {
		if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations SET observed_at=?,type_tokens=?,updated_at=`+s.dialect.Now()+` WHERE id=?`, observedAt, joinTypeTokens(provider.Envelope.TypeTokens), prior.Envelope.ID); err != nil {
			return 0, err
		}
		if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
			return 0, err
		}
	}
	renamed := false
	if input.Protected {
		for _, o := range observations {
			if err := s.supersedeObservationRowTx(ctx, tx, o.Envelope.ID); err != nil {
				return 0, err
			}
			renamed = true
		}
	} else {
		service, _, err := resolveCommunicationServiceTx(ctx, tx, new("x"))
		if err != nil {
			return 0, err
		}
		normalized, err := NormalizeServiceValue(service, ContactAddressUsername, input.Username)
		if err != nil {
			return 0, err
		}
		for _, o := range observations {
			if o.ObservedAt != nil && observedAt.Equal(*o.ObservedAt) && o.NormalizedValue != normalized {
				return pid, nil
			}
		}
		for _, o := range observations {
			if o.NormalizedValue != normalized {
				if err := s.supersedeObservationRowTx(ctx, tx, o.Envelope.ID); err != nil {
					return 0, err
				}
				renamed = true
			}
		}
		username := base
		username.AddressKind = ContactAddressUsername
		username.OriginalValue = input.Username
		result, err := s.recordContactObservationTx(ctx, tx, pid, username)
		if err != nil {
			return 0, err
		}
		if result.Observation.Envelope.Source == ProvenanceArchiveObservation && (result.Observation.ObservedAt == nil || observedAt.After(*result.Observation.ObservedAt)) {
			if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations SET observed_at=?,updated_at=`+s.dialect.Now()+` WHERE id=?`, observedAt, result.Observation.Envelope.ID); err != nil {
				return 0, err
			}
			if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
				return 0, err
			}
		}
		if input.DisplayName != "" {
			if _, err := s.ensureParticipantByIdentifierTx(ctx, tx, "x_user_id", input.UserID, input.DisplayName); err != nil {
				return 0, err
			}
		}
	}
	if renamed {
		if err := s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
			return 0, err
		}
		if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
			return 0, err
		}
	}
	return pid, nil
}
