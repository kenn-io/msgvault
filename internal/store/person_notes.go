package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PersonNoteAppendInput appends one fragment to a person's standard Notes value.
type PersonNoteAppendInput struct {
	PersonID   int64
	Text       string
	Source     Provenance
	SourceRef  *string
	Confidence *float64
	Actor      *string
	DryRun     bool
}

// AppendPersonNoteContext atomically appends a fragment to the standard Notes
// value while preserving attribute history.
func (s *Store) AppendPersonNoteContext(
	ctx context.Context, input PersonNoteAppendInput,
) (*PersonAttributeWrite, error) {
	if err := validateProvenance(input.Source, input.Confidence); err != nil {
		return nil, err
	}
	return retryContendedWrite(ctx, s, "append person note",
		func() (*PersonAttributeWrite, error) {
			return s.appendPersonNoteOnce(ctx, input)
		})
}

func (s *Store) appendPersonNoteOnce(
	ctx context.Context, input PersonNoteAppendInput,
) (*PersonAttributeWrite, error) {
	write := &PersonAttributeWrite{DryRun: input.DryRun}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		definition, err := s.getAttributeDefinitionByUniversalIDTx(
			ctx, tx, AttributeUniversalIDNotes,
		)
		if err != nil {
			return err
		}
		if err := writableAttributeDefinition(*definition); err != nil {
			return err
		}
		if definition.ValueType != AttributeValueText ||
			definition.FieldType != AttributeFieldTextarea ||
			definition.Cardinality != AttributeCardinalitySingle {
			return fmt.Errorf(
				"%s must remain a single-cardinality text textarea: %w",
				definition.Slug, ErrAttributeDefinitionNotWritable,
			)
		}

		var personExists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM persons WHERE id = ?`, input.PersonID,
		).Scan(&personExists); err != nil {
			return fmt.Errorf("verify person %d: %w", input.PersonID, err)
		}
		if personExists == 0 {
			return ErrPersonNotFound
		}

		current, hasCurrent, err := s.currentPersonAttributeValueTx(
			ctx, tx, input.PersonID, definition.ID, 0,
		)
		if err != nil {
			return err
		}
		if strings.TrimSpace(input.Text) == "" {
			return fmt.Errorf("%w: note fragment must not be blank", ErrAttributeValueInvalid)
		}
		fragment := input.Text
		combined := fragment
		if hasCurrent {
			if current.Value.Text == nil {
				return fmt.Errorf("%w: current notes value is not text", ErrAttributeValueInvalid)
			}
			combined = *current.Value.Text + "\n" + fragment
		}
		value, err := normalizeAttributeValue(*definition, AttributeValue{
			Type: AttributeValueText, Text: &combined,
		})
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		activeFrom := now
		if hasCurrent && current.ActiveFrom.After(activeFrom) {
			activeFrom = current.ActiveFrom
		}
		if hasCurrent {
			closed, err := s.closePersonAttributeValueTx(
				ctx, tx, current.ID, activeFrom, now,
			)
			if err != nil {
				return err
			}
			write.Superseded = closed
		}
		ordinal := int64(0)
		inserted, err := s.insertPersonAttributeValueTx(
			ctx, tx, *definition, PersonAttributeValueInput{
				PersonID: input.PersonID, DefinitionSlug: definition.Slug,
				Ordinal: &ordinal, Value: value, Source: input.Source,
				SourceRef: input.SourceRef, Confidence: input.Confidence, Actor: input.Actor,
			}, ordinal, activeFrom,
		)
		if err != nil {
			return err
		}
		write.Value = inserted
		if err := s.bumpPersonVCardProjectionsTx(ctx, tx, input.PersonID); err != nil {
			return err
		}
		if input.DryRun {
			write.Value.ID = 0
			if write.Superseded != nil {
				write.Superseded.ID = current.ID
			}
			return errAttributeDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errAttributeDryRun) {
		return nil, err
	}
	return write, nil
}
