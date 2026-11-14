package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrAttributeObjectTypeMismatch reports a definition scoped to another owner type.
var ErrAttributeObjectTypeMismatch = errors.New("attribute definition object type mismatch")

// OrganizationAttributeValue is one typed value and its history metadata.
type OrganizationAttributeValue struct {
	ID             int64          `json:"id"`
	OrganizationID int64          `json:"organization_id"`
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

// OrganizationAttributeValueInput sets one typed organization attribute value.
type OrganizationAttributeValueInput struct {
	OrganizationID  int64
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

// OrganizationAttributeSupersedeInput closes one current value without replacement.
type OrganizationAttributeSupersedeInput struct {
	OrganizationID  int64
	DefinitionSlug  string
	Ordinal         *int64
	At              *time.Time
	Actor           *string
	ExpectedValueID *int64
	DryRun          bool
}

// OrganizationAttributeWrite describes a set or supersede result.
type OrganizationAttributeWrite struct {
	Value      *OrganizationAttributeValue `json:"value,omitzero" nullable:"false"`
	Superseded *OrganizationAttributeValue `json:"superseded,omitzero" nullable:"false"`
	DryRun     bool                        `json:"dry_run"`
}

// OrganizationAttributeQuery filters an organization's attribute values.
type OrganizationAttributeQuery struct {
	DefinitionSlug string
	IncludeHistory bool
}

// organizationAttributeOwner locks the organization row after the definition
// checks and rejects writes through a merged organization's redirect.
var organizationAttributeOwner = attributeOwner{
	objectType: AttributeObjectOrganization, noun: "organization",
	table: "organization_attribute_values", column: "organization_id",
	lockOwner: func(ctx context.Context, s *Store, tx *loggedTx, organizationID int64, _ bool) error {
		organization, err := getOrganizationForUpdateTx(ctx, tx, s.dialect, organizationID)
		if err != nil {
			return err
		}
		if organization.MergedIntoID != nil {
			return fmt.Errorf("%w: merged organization redirects are immutable",
				ErrOrganizationInvalid)
		}
		return nil
	},
	conflict: func(*attributeValueRow) error { return ErrAttributeValueConflict },
}

// ListOrganizationAttributeValuesContext lists current or historical values.
func (s *Store) ListOrganizationAttributeValuesContext(
	ctx context.Context, organizationID int64, query OrganizationAttributeQuery,
) ([]OrganizationAttributeValue, error) {
	rows, err := s.listAttributeValuesContext(ctx, s.db, organizationAttributeOwner,
		organizationID, query.DefinitionSlug, query.IncludeHistory)
	if err != nil {
		return nil, err
	}
	return attributeRowsAs(rows, (*attributeValueRow).organization), nil
}

// SetOrganizationAttributeValueContext supersedes the current value and inserts a replacement.
func (s *Store) SetOrganizationAttributeValueContext(
	ctx context.Context, input OrganizationAttributeValueInput,
) (*OrganizationAttributeWrite, error) {
	if err := validateProvenance(input.Source, input.Confidence); err != nil {
		return nil, err
	}
	if err := validateAttributeOrdinal(input.Ordinal); err != nil {
		return nil, err
	}
	if _, err := attributeActiveFrom(input.ActiveFrom, input.ActiveUntil, time.Now().UTC()); err != nil {
		return nil, err
	}
	write, err := s.runAttributeWrite(ctx, "set organization attribute value", input.DryRun,
		func(tx *loggedTx, now time.Time) (*attributeValueWrite, error) {
			activeFrom, err := attributeActiveFrom(input.ActiveFrom, input.ActiveUntil, now)
			if err != nil {
				return nil, err
			}
			definition, err := s.getOrganizationAttributeDefinitionTx(ctx, tx, input.DefinitionSlug)
			if err != nil {
				return nil, err
			}
			return s.setAttributeValueTx(ctx, tx, organizationAttributeOwner, *definition,
				attributeValueInput{
					OwnerID: input.OrganizationID, Ordinal: input.Ordinal, Value: input.Value,
					ActiveUntil: input.ActiveUntil, Source: input.Source, SourceRef: input.SourceRef,
					Confidence: input.Confidence, Actor: input.Actor, ExpectedValueID: input.ExpectedValueID,
				}, activeFrom, now)
		})
	return write.organization(), err
}

// SupersedeOrganizationAttributeValueContext closes a current value without replacement.
func (s *Store) SupersedeOrganizationAttributeValueContext(
	ctx context.Context, input OrganizationAttributeSupersedeInput,
) (*OrganizationAttributeWrite, error) {
	if err := validateAttributeOrdinal(input.Ordinal); err != nil {
		return nil, err
	}
	at := time.Now().UTC()
	if input.At != nil {
		at = input.At.UTC()
	}
	write, err := s.runAttributeWrite(ctx, "supersede organization attribute value", input.DryRun,
		func(tx *loggedTx, now time.Time) (*attributeValueWrite, error) {
			definition, err := s.getOrganizationAttributeDefinitionTx(ctx, tx, input.DefinitionSlug)
			if err != nil {
				return nil, err
			}
			closed, err := s.supersedeAttributeValueTx(ctx, tx, organizationAttributeOwner, *definition,
				input.OrganizationID, input.Ordinal, input.ExpectedValueID, at, now)
			if err != nil {
				return nil, err
			}
			return &attributeValueWrite{Superseded: closed}, nil
		})
	return write.organization(), err
}

func (s *Store) getOrganizationAttributeDefinitionTx(
	ctx context.Context, tx *loggedTx, slug string,
) (*AttributeDefinition, error) {
	definition, err := scanAttributeDefinition(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM attribute_definitions
		WHERE object_type = ? AND slug = ?%s
	`, attributeDefinitionColumns, s.dialect.SelectForUpdate()),
		string(AttributeObjectOrganization), slug))
	if err == nil {
		return definition, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get organization attribute definition %q: %w", slug, err)
	}
	var objectType AttributeObjectType
	scopeErr := tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT object_type FROM attribute_definitions
		WHERE slug = ? ORDER BY id LIMIT 1%s
	`, s.dialect.SelectForUpdate()), slug).Scan(&objectType)
	if errors.Is(scopeErr, sql.ErrNoRows) {
		return nil, ErrAttributeDefinitionNotFound
	}
	if scopeErr != nil {
		return nil, fmt.Errorf("check attribute definition scope: %w", scopeErr)
	}
	return nil, fmt.Errorf(
		"%w: definition %q is scoped to %s, not organization",
		ErrAttributeObjectTypeMismatch, slug, objectType)
}
