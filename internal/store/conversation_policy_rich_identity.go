package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ConversationParticipantIdentity contains already-fetched provider metadata.
// Directory/network lookup belongs to the caller, outside the policy fence.
type ConversationParticipantIdentity struct {
	IdentifierType  string
	IdentifierValue string
	DisplayName     string
	Email           string
	Phone           string
}

type ConversationParticipantResolution struct {
	ParticipantID int64
	ByPhone       bool
}

// ResolveConversationParticipantContext admits all automatic rich identity
// writes against their actual originating room in one caller transaction.
// ErrConversationPolicyStale means the importer should retain text using a bare
// archival identifier. No partial rich row, merge or mapping survives an error.
func (s *Store) ResolveConversationParticipantContext(ctx context.Context, room int64, input ConversationParticipantIdentity) (ConversationParticipantResolution, error) {
	input.IdentifierType = strings.TrimSpace(input.IdentifierType)
	input.IdentifierValue = strings.TrimSpace(input.IdentifierValue)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if input.IdentifierType == "" || input.IdentifierValue == "" {
		return ConversationParticipantResolution{}, errors.New("identifier type and value are required")
	}
	var result ConversationParticipantResolution
	err := s.withPolicyAttributionTxContext(ctx, func(tx *loggedTx) error {
		if err := s.CheckConversationPolicyPublicationTx(ctx, tx.Tx, nil); err != nil {
			return err
		}
		var full bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND `+FullConversationWhere("c")+`)`, room).Scan(&full); err != nil {
			return err
		}
		if !full {
			return ErrConversationPolicyStale
		}
		recordAdmittedObservations := func() error {
			var sourceID int64
			if err := tx.QueryRowContext(ctx, `SELECT source_id FROM conversations WHERE id=?`, room).Scan(&sourceID); err != nil {
				return err
			}
			if input.Email != "" {
				if err := s.recordProviderEmailObservationTx(ctx, tx, result.ParticipantID, sourceID, room, input.Email); err != nil {
					return err
				}
			}
			if input.IdentifierType != "beeper" {
				var serviceSlug *string
				var scopeKind, scopeValue *string
				if classification, ok := classifyParticipantIdentifier(input.IdentifierType, input.IdentifierValue); ok {
					serviceSlug = &classification.ServiceSlug
					scopeKind = classification.ScopeKind
					scopeValue = classification.ScopeValue
				}
				if err := s.recordConversationPolicyParticipantObservationTx(
					ctx, tx, result.ParticipantID, sourceID, room,
					ContactAddressProviderIdentity, serviceSlug, scopeKind, scopeValue,
					&input.IdentifierValue, input.IdentifierValue,
				); err != nil {
					return err
				}
			}
			if input.DisplayName != "" {
				var displayName sql.NullString
				if err := tx.QueryRowContext(ctx, `SELECT display_name FROM participants WHERE id=?`, result.ParticipantID).Scan(&displayName); err != nil {
					return err
				}
				if displayName.Valid && displayName.String == input.DisplayName {
					return s.recordConversationPolicyParticipantObservationTx(
						ctx, tx, result.ParticipantID, sourceID, room,
						ContactAddressDisplayName, nil, nil, nil, nil, input.DisplayName,
					)
				}
			}
			return nil
		}
		var previous int64
		var previousPhone string
		err := tx.QueryRowContext(ctx, `SELECT p.id,COALESCE(p.phone_number,'') FROM participant_identifiers pi JOIN participants p ON p.id=pi.participant_id WHERE pi.identifier_type=? AND pi.identifier_value=?`, input.IdentifierType, input.IdentifierValue).Scan(&previous, &previousPhone)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Existing phone resolution is never downgraded by an email-only sighting.
		if input.Phone == "" && previousPhone != "" {
			result = ConversationParticipantResolution{ParticipantID: previous, ByPhone: true}
			return recordAdmittedObservations()
		}
		target := previous
		switch {
		case input.Phone != "":
			err = tx.QueryRowContext(ctx, `SELECT id FROM participants WHERE phone_number=?`, input.Phone).Scan(&target)
		case input.Email != "":
			err = tx.QueryRowContext(ctx, `SELECT id FROM participants WHERE email_address=?`, input.Email).Scan(&target)
		}
		if errors.Is(err, sql.ErrNoRows) {
			target = 0
		} else if err != nil {
			return err
		}
		// Repeated sightings can still restore exact-room email support. Once the
		// mapping is stable and no profile write is needed, skip historical capture.
		if input.Phone == "" && previous != 0 && target == previous {
			stable := true
			if input.Email == "" && input.DisplayName != "" {
				var displayName sql.NullString
				if err := tx.QueryRowContext(ctx, `SELECT display_name FROM participants WHERE id=?`, previous).Scan(&displayName); err != nil {
					return err
				}
				stable = displayName.Valid && displayName.String != ""
			}
			if input.Email != "" {
				normalizedEmail, err := NormalizeServiceValue(nil, ContactAddressEmail, input.Email)
				if err != nil {
					return err
				}
				var sourceID int64
				if err := tx.QueryRowContext(ctx, `SELECT source_id FROM conversations WHERE id=?`, room).Scan(&sourceID); err != nil {
					return err
				}
				var observationExists bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM participant_contact_observations
					WHERE participant_id=? AND source_id=? AND address_kind='email' AND normalized_value=?)`,
					previous, sourceID, normalizedEmail).Scan(&observationExists); err != nil {
					return err
				}
				stable = observationExists
			}
			if stable {
				result = ConversationParticipantResolution{ParticipantID: previous}
				return recordAdmittedObservations()
			}
		}
		state, err := s.captureParticipantConversationPolicyTx(ctx, tx, previous, target)
		if err != nil {
			return err
		}
		before, err := conversationParticipantMutationRevision(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case input.Phone != "":
			result.ParticipantID, err = s.ensureParticipantByPhoneTx(ctx, tx, input.Phone, input.DisplayName, input.IdentifierType)
			result.ByPhone = true
		case input.Email != "":
			_, domain, _ := strings.Cut(input.Email, "@")
			result.ParticipantID, err = ensureParticipantWith(boundQuerier{ctx: ctx, q: tx}, s.dialect, input.Email, input.DisplayName, domain, func() error { return s.bumpParticipantDisplayNameRevisionContext(ctx, tx) })
		default:
			result.ParticipantID, err = s.ensureParticipantByIdentifierTx(ctx, tx, input.IdentifierType, input.IdentifierValue, input.DisplayName)
		}
		if err != nil {
			return err
		}
		// Different phone owners can represent recycled numbers. Keep both histories
		// while allowing an admitted sighting to update the provider mapping.
		if previous != 0 && previous != result.ParticipantID && previousPhone == "" {
			if err = s.mergeParticipantsTx(ctx, tx, previous, result.ParticipantID); err != nil {
				return err
			}
		}
		if err = s.setParticipantIdentifierTx(ctx, tx, result.ParticipantID, input.IdentifierType, input.IdentifierValue); err != nil {
			return err
		}
		if err = recordAdmittedObservations(); err != nil {
			return err
		}
		after, err := conversationParticipantMutationRevision(ctx, tx)
		if err != nil {
			return err
		}
		if before == after {
			return nil
		}
		return s.finishConversationPolicyLifecycleTx(ctx, tx, state)
	})
	if err != nil {
		return ConversationParticipantResolution{}, err
	}
	return result, nil
}

// Existing monotonic revision clocks distinguish an idempotent sighting from
// an actual identity/name mutation without adding another durable registry.
func conversationParticipantMutationRevision(ctx context.Context, tx *loggedTx) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key IN ('identity_revision','account_identity_revision','participant_identifier_revision','participant_display_name_revision')`).Scan(&revision)
	return revision, err
}
