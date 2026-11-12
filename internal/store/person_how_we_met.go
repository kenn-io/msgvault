package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

const firstAppearanceSourcePrefix = "first-appearance:"

func renderHowWeMet(first *firstAppearance) string {
	if first == nil {
		return ""
	}
	month := first.event.OccurredAt.UTC().Format("January 2006")
	channel := "chat"
	if first.candidate.ConversationType == "email_thread" {
		channel = "email"
	}
	switch first.kind {
	case FirstContactEmailIntro:
		name := "Someone"
		if first.introducer != nil {
			trimmed := []rune(strings.TrimSpace(first.introducer.DisplayName))
			if len(trimmed) > 120 {
				trimmed = trimmed[:120]
			}
			if len(trimmed) > 0 {
				name = string(trimmed)
			}
		}
		return fmt.Sprintf("%s introduced them by email in %s.", name, month)
	case FirstContactOwnerOutreach:
		return fmt.Sprintf("In your archive, you contacted them first by %s in %s.", channel, month)
	case FirstContactInboundOutreach:
		return fmt.Sprintf("In your archive, they contacted you first by %s in %s.", channel, month)
	case FirstContactGroupShared:
		return fmt.Sprintf("In your archive, they first appeared in a group conversation in %s.", month)
	case FirstContactMeeting:
		return fmt.Sprintf("In your archive, they first appeared in a meeting in %s.", month)
	default:
		return ""
	}
}

// FillPersonHowWeMetContext reconciles deterministic first appearances without
// recording a model claim or changing manual pin state.
func (s *Store) FillPersonHowWeMetContext(ctx context.Context, request peoplesweep.HowWeMetRequest) error {
	after, before, err := sweepDateBounds(request.SourceSince, request.SourceUntil)
	if err != nil {
		return err
	}
	for _, personID := range request.PersonIDs {
		text, ref := "", ""
		err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
			first, err := s.firstAppearanceTx(ctx, tx, personID)
			if err != nil {
				return err
			}
			if first != nil && (after == nil || !first.event.OccurredAt.Before(*after)) && (before == nil || first.event.OccurredAt.Before(*before)) {
				var allowed bool
				query := `SELECT EXISTS(SELECT 1 FROM messages m WHERE m.id = ? AND (` + personSweepHistoricalLanePredicate(request.AllowedSources) + `))`
				if err := tx.QueryRowContext(ctx, query, first.event.MessageID).Scan(&allowed); err != nil {
					return err
				}
				if allowed {
					text = renderHowWeMet(first)
					ref = fmt.Sprintf("%s%s:%d", firstAppearanceSourcePrefix, first.kind, first.event.MessageID)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("fill how we met for person %d: %w", personID, err)
		}
		_, err = s.runAttributeWrite(ctx, "fill how we met", false, func(tx *loggedTx, _ time.Time) (*attributeValueWrite, error) {
			empty := &attributeValueWrite{}
			var slug string
			err := tx.QueryRowContext(ctx, `SELECT slug FROM attribute_definitions WHERE universal_id = ? AND object_type = ?`, AttributeUniversalIDHowWeMet, string(AttributeObjectPerson)).Scan(&slug)
			if err != nil {
				return nil, err
			}
			if err := s.lockPersonFactAttributeTx(ctx, tx, personID, slug); err != nil {
				return nil, err
			}
			if err := verifyTrackedPersonFactPersonTx(ctx, tx, personID); errors.Is(err, ErrPersonFactPersonNotTracked) {
				return empty, nil
			} else if err != nil {
				return nil, err
			}
			active, err := s.hasActivePersonInferenceConsentTx(ctx, tx, request.ProviderFingerprint)
			if err != nil {
				return nil, err
			}
			if !active {
				return empty, nil
			}
			definition, err := s.getAttributeDefinitionByUniversalIDTx(ctx, tx, AttributeUniversalIDHowWeMet)
			if err != nil {
				return nil, err
			}
			if !definition.IsActive {
				return empty, nil
			}
			descriptor, err := personFactAttributeDescriptor(*definition)
			if err != nil {
				return nil, err
			}
			pin, err := s.effectivePersonFactPinTx(ctx, tx, personID, descriptor, nil)
			if err != nil {
				return nil, err
			}
			if pin.Pinned {
				return empty, nil
			}
			current, found, err := s.currentPersonAttributeValueTx(ctx, tx, personID, definition.ID, 0)
			if err != nil {
				return nil, err
			}
			ours := found && current.Source == ProvenanceSystem && current.SourceRef != nil && strings.HasPrefix(*current.SourceRef, firstAppearanceSourcePrefix)
			if found && !ours {
				return empty, nil
			}
			if text == "" && !ours {
				return empty, nil
			}
			if ours && text != "" && current.Value.Text != nil && *current.Value.Text == text && *current.SourceRef == ref {
				return empty, nil
			}
			now := time.Now().UTC()
			// A future-dated current row still needs a valid history interval.
			if ours && now.Before(current.ActiveFrom) {
				now = current.ActiveFrom
			}
			if text == "" {
				if _, err := s.closePersonAttributeValueTx(ctx, tx, current.ID, now, now); err != nil {
					return nil, err
				}
			} else {
				input := PersonAttributeValueInput{PersonID: personID, DefinitionSlug: definition.Slug, Value: AttributeValue{Type: AttributeValueText, Text: &text}, Source: ProvenanceSystem, SourceRef: &ref}
				if ours {
					input.ExpectedValueID = &current.ID
				}
				if _, err := s.setPersonAttributeValueTx(ctx, tx, *definition, input, now, now); err != nil {
					return nil, err
				}
			}
			if err := s.bumpPersonVCardProjectionsTx(ctx, tx, personID); err != nil {
				return nil, err
			}
			return empty, nil
		})
		if err != nil {
			return fmt.Errorf("fill how we met for person %d: %w", personID, err)
		}
	}
	return nil
}
