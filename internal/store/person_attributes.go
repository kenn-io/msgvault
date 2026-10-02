package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// PersonAttributeValue is one typed value and its history metadata.
type PersonAttributeValue struct {
	ID             int64          `json:"id"`
	PersonID       int64          `json:"person_id"`
	DefinitionID   int64          `json:"definition_id"`
	DefinitionSlug string         `json:"definition_slug"`
	Ordinal        int64          `json:"ordinal"`
	Value          AttributeValue `json:"value"`
	ActiveFrom     time.Time      `json:"active_from"`
	ActiveUntil    *time.Time     `json:"active_until,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	SupersededAt   *time.Time     `json:"superseded_at,omitempty"`
	Source         Provenance     `json:"source"`
	SourceRef      *string        `json:"source_ref,omitzero" nullable:"false"`
	Confidence     *float64       `json:"confidence,omitzero" nullable:"false"`
	Actor          *string        `json:"actor,omitzero" nullable:"false"`
}

// PersonAttributeValueInput sets one typed person attribute value.
type PersonAttributeValueInput struct {
	PersonID        int64
	DefinitionSlug  string
	Ordinal         *int64
	Value           AttributeValue
	ActiveFrom      *time.Time
	ActiveUntil     *time.Time
	Source          Provenance
	SourceRef       *string
	Confidence      *float64
	Actor           *string
	ExpectedValueID *int64
	DryRun          bool
}

// PersonAttributeSupersedeInput closes one current value without replacement.
type PersonAttributeSupersedeInput struct {
	PersonID        int64
	DefinitionSlug  string
	Ordinal         *int64
	At              *time.Time
	Actor           *string
	ExpectedValueID *int64
	DryRun          bool
}

// PersonAttributeWrite describes a set or supersede result.
type PersonAttributeWrite struct {
	Value      *PersonAttributeValue `json:"value,omitzero" nullable:"false"`
	Superseded *PersonAttributeValue `json:"superseded,omitzero" nullable:"false"`
	DryRun     bool                  `json:"dry_run"`
}

// PersonAttributeQuery filters a person's attribute values.
type PersonAttributeQuery struct {
	DefinitionSlug string
	IncludeHistory bool
}

var personAttributeValueColumns = personAttributeOwner.columns()

// personAttributeOwner checks existence only on set: a supersede on a missing
// person reports the missing value instead.
var personAttributeOwner = attributeOwner{
	objectType: AttributeObjectPerson, noun: "person",
	table: "person_attribute_values", column: personMergePersonIDColumn, stampCreatedAt: true,
	lockOwner: func(ctx context.Context, _ *Store, tx *loggedTx, personID int64, set bool) error {
		if !set {
			return nil
		}
		return ensureProfilePersonTx(ctx, tx, personID)
	},
	conflict: func(current *attributeValueRow) error {
		return &AttributeValueConflictError{CurrentValue: current.person()}
	},
}

func personAttributeInput(input PersonAttributeValueInput) attributeValueInput {
	return attributeValueInput{
		OwnerID: input.PersonID, Ordinal: input.Ordinal, Value: input.Value,
		ActiveUntil: input.ActiveUntil, Source: input.Source, SourceRef: input.SourceRef,
		Confidence: input.Confidence, Actor: input.Actor, ExpectedValueID: input.ExpectedValueID,
	}
}

// ListPersonAttributeValuesContext lists current or historical values.
func (s *Store) ListPersonAttributeValuesContext(
	ctx context.Context, personID int64, query PersonAttributeQuery,
) ([]PersonAttributeValue, error) {
	return s.listPersonAttributeValuesContext(ctx, s.db, personID, query)
}

func (s *Store) listPersonAttributeValuesContext(
	ctx context.Context, queryer contextRowsQuerier,
	personID int64, query PersonAttributeQuery,
) ([]PersonAttributeValue, error) {
	rows, err := s.listAttributeValuesContext(ctx, queryer, personAttributeOwner,
		personID, query.DefinitionSlug, query.IncludeHistory)
	if err != nil {
		return nil, err
	}
	return attributeRowsAs(rows, (*attributeValueRow).person), nil
}

// SetPersonAttributeValueContext supersedes the current value and inserts a
// replacement. Definition-dependent validation runs inside each write
// attempt's transaction so a concurrent definition change cannot slip
// between check and insert.
func (s *Store) SetPersonAttributeValueContext(
	ctx context.Context, input PersonAttributeValueInput,
) (*PersonAttributeWrite, error) {
	if err := validateProvenance(input.Source, input.Confidence); err != nil {
		return nil, err
	}
	if err := validateAttributeOrdinal(input.Ordinal); err != nil {
		return nil, err
	}
	if _, err := attributeActiveFrom(input.ActiveFrom, input.ActiveUntil, time.Now().UTC()); err != nil {
		return nil, err
	}
	write, err := s.runAttributeWrite(ctx, "set person attribute value", input.DryRun,
		func(tx *loggedTx, now time.Time) (*attributeValueWrite, error) {
			activeFrom, err := attributeActiveFrom(input.ActiveFrom, input.ActiveUntil, now)
			if err != nil {
				return nil, err
			}
			if err := s.lockPersonFactAttributeTx(
				ctx, tx, input.PersonID, input.DefinitionSlug); err != nil {
				return nil, err
			}
			definition, err := s.getAttributeDefinitionBySlugTx(
				ctx, tx, AttributeObjectPerson, input.DefinitionSlug)
			if err != nil {
				return nil, err
			}
			if err := s.lockEmploymentPeopleTx(ctx, tx, input.PersonID); err != nil {
				return nil, err
			}
			var inferenceProjectionBefore map[int64]personInferenceExportProjection
			if provenanceIsInferred(input.Source) {
				inferenceProjectionBefore, err = s.captureInferenceExportPeopleTx(ctx, tx, input.PersonID)
				if err != nil {
					return nil, fmt.Errorf("load inference export projection before attribute write: %w", err)
				}
			}
			write, err := s.setAttributeValueTx(ctx, tx, personAttributeOwner,
				*definition, personAttributeInput(input), activeFrom, now)
			if err != nil {
				return nil, err
			}
			if input.Source.IsDeclared() {
				if err := s.appendManualPersonFactAttributePinTx(
					ctx, tx, *definition, input.PersonID, personAttributeActor(input.Actor, input.Source)); err != nil {
					return nil, err
				}
			}
			if err := s.bumpPersonVCardProjectionsTx(ctx, tx, input.PersonID); err != nil {
				return nil, err
			}
			if provenanceIsInferred(input.Source) {
				if err := s.invalidateInferenceExportChangesTx(ctx, tx, inferenceProjectionBefore); err != nil {
					return nil, err
				}
			}
			return write, nil
		})
	return write.person(), err
}

func (s *Store) setPersonAttributeValueTx(
	ctx context.Context, tx *loggedTx, definition AttributeDefinition,
	input PersonAttributeValueInput, activeFrom time.Time, transactionTime time.Time,
) (*PersonAttributeWrite, error) {
	write, err := s.setAttributeValueTx(ctx, tx, personAttributeOwner,
		definition, personAttributeInput(input), activeFrom, transactionTime)
	if err != nil {
		return nil, err
	}
	write.DryRun = input.DryRun
	return write.person(), nil
}

func personAttributeActor(actor *string, source Provenance) string {
	if actor != nil && strings.TrimSpace(*actor) != "" {
		return strings.TrimSpace(*actor)
	}
	return string(source)
}

func (s *Store) currentPersonAttributeValueTx(
	ctx context.Context, tx *loggedTx, personID, definitionID, ordinal int64,
) (*PersonAttributeValue, bool, error) {
	value, found, err := s.currentAttributeValueTx(
		ctx, tx, personAttributeOwner, personID, definitionID, ordinal)
	return value.person(), found, err
}

func (s *Store) closePersonAttributeValueTx(
	ctx context.Context, tx *loggedTx, valueID int64,
	activeUntil, supersededAt time.Time,
) (*PersonAttributeValue, error) {
	closed, err := s.closeAttributeValueTx(
		ctx, tx, personAttributeOwner, valueID, activeUntil, supersededAt)
	return closed.person(), err
}

func (s *Store) insertPersonAttributeValueTx(
	ctx context.Context, tx *loggedTx, definition AttributeDefinition,
	input PersonAttributeValueInput, ordinal int64, activeFrom time.Time,
	transactionTime time.Time,
) (*PersonAttributeValue, error) {
	inserted, err := s.insertAttributeValueTx(ctx, tx, personAttributeOwner,
		definition, personAttributeInput(input), ordinal, activeFrom, transactionTime)
	return inserted.person(), err
}

// SupersedePersonAttributeValueContext closes a current value without
// replacement. Unlike Set it also works on inactive definitions: retracting a
// stale value from a retired definition must stay possible. As in Set, the
// definition is loaded and checked inside each attempt's transaction.
func (s *Store) SupersedePersonAttributeValueContext(
	ctx context.Context, input PersonAttributeSupersedeInput,
) (*PersonAttributeWrite, error) {
	if err := validateAttributeOrdinal(input.Ordinal); err != nil {
		return nil, err
	}
	at := time.Now().UTC()
	if input.At != nil {
		at = input.At.UTC()
	}
	write, err := s.runAttributeWrite(ctx, "supersede person attribute value", input.DryRun,
		func(tx *loggedTx, now time.Time) (*attributeValueWrite, error) {
			if err := s.lockPersonFactAttributeTx(
				ctx, tx, input.PersonID, input.DefinitionSlug); err != nil {
				return nil, err
			}
			definition, err := s.getAttributeDefinitionBySlugTx(
				ctx, tx, AttributeObjectPerson, input.DefinitionSlug)
			if err != nil {
				return nil, err
			}
			closed, err := s.supersedeAttributeValueTx(ctx, tx, personAttributeOwner, *definition,
				input.PersonID, input.Ordinal, input.ExpectedValueID, at, now)
			if err != nil {
				return nil, err
			}
			if err := s.appendManualPersonFactAttributePinTx(
				ctx, tx, *definition, input.PersonID, personAttributeActor(input.Actor, ProvenanceUser)); err != nil {
				return nil, err
			}
			if err := s.bumpPersonVCardProjectionsTx(ctx, tx, input.PersonID); err != nil {
				return nil, err
			}
			return &attributeValueWrite{Superseded: closed}, nil
		})
	return write.person(), err
}

// lockPersonFactAttributeTx joins attribute writes to the same
// generation-then-target lock order used by automatic fact resolution. The
// initial definition lookup is intentionally unlocked: taking its row lock
// before these advisory locks would invert the automatic resolver's order.
func (s *Store) lockPersonFactAttributeTx(
	ctx context.Context, tx *loggedTx, personID int64, definitionSlug string,
) error {
	if err := s.lockProfileIdentityKeyTxContext(
		ctx, tx, "person-fact-generation", personID); err != nil {
		return err
	}
	var targetKey string
	err := tx.QueryRowContext(ctx, `
		SELECT universal_id FROM attribute_definitions
		WHERE object_type = ? AND slug = ?
	`, string(AttributeObjectPerson), definitionSlug).Scan(&targetKey)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAttributeDefinitionNotFound
	}
	if err != nil {
		return fmt.Errorf("resolve person fact target for attribute %s: %w", definitionSlug, err)
	}
	return s.lockProfileIdentityKeyTxContext(
		ctx, tx, "person-fact-target", personID, personfacts.TargetAttribute, targetKey)
}

func personFactAttributeTargetRef(definition AttributeDefinition) (personfacts.TargetRef, error) {
	descriptor, err := personFactAttributeDescriptor(definition)
	if err != nil {
		return personfacts.TargetRef{}, err
	}
	return personfacts.TargetRef{
		Kind: descriptor.Kind, Key: descriptor.Key, Revision: descriptor.Revision,
	}, nil
}

func scanPersonAttributeValue(row scanner) (*PersonAttributeValue, error) {
	value, err := scanAttributeValueRow(row)
	if err != nil {
		return nil, err
	}
	return value.person(), nil
}
