package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/jsonexact"
)

var (
	ErrAttributeDefinitionNotFound     = errors.New("attribute definition not found")
	ErrAttributeDefinitionInvalid      = errors.New("invalid attribute definition")
	ErrAttributeDefinitionSlugConflict = errors.New(
		"attribute definition slug already exists for this object type")
	ErrAttributeDefinitionUniversalIDConflict = errors.New(
		"attribute definition universal id already exists")
	ErrAttributeDefinitionRevisionConflict = errors.New(
		"attribute definition revision conflict")
	ErrAttributeDefinitionNotDeletable = errors.New(
		"attribute definition is not deletable")
	ErrAttributeDefinitionHasValues = errors.New(
		"attribute definition still has stored values")
	// ErrAttributeUniquenessUnsupported rejects decorative uniqueness metadata.
	ErrAttributeUniquenessUnsupported = errors.New(
		"attribute definition uniqueness is not supported: " +
			"a uniqueness claim must be backed by a portable database index")
)

// AttributeObjectType identifies the kind of record a definition describes.
type AttributeObjectType string

const (
	AttributeObjectPerson       AttributeObjectType = "person"
	AttributeObjectOrganization AttributeObjectType = "organization"
)

// AttributeValueType identifies a definition's storage type.
type AttributeValueType string

const (
	AttributeValueText            AttributeValueType = "text"
	AttributeValueInteger         AttributeValueType = "integer"
	AttributeValueReal            AttributeValueType = "real"
	AttributeValueBoolean         AttributeValueType = "boolean"
	AttributeValueDate            AttributeValueType = "date"
	AttributeValueTimestamp       AttributeValueType = "timestamp"
	AttributeValueJSON            AttributeValueType = "json"
	AttributeValueRecordReference AttributeValueType = "record_reference"
)

// AttributeFieldType identifies a definition's presentation widget.
type AttributeFieldType string

const (
	AttributeFieldText         AttributeFieldType = "text"
	AttributeFieldTextarea     AttributeFieldType = "textarea"
	AttributeFieldSelect       AttributeFieldType = "select"
	AttributeFieldMultiselect  AttributeFieldType = "multiselect"
	AttributeFieldCheckbox     AttributeFieldType = "checkbox"
	AttributeFieldDate         AttributeFieldType = "date"
	AttributeFieldTimestamp    AttributeFieldType = "timestamp"
	AttributeFieldDuration     AttributeFieldType = "duration"
	AttributeFieldPerson       AttributeFieldType = "person"
	AttributeFieldOrganization AttributeFieldType = "organization"
	AttributeFieldURL          AttributeFieldType = "url"
	AttributeFieldEmail        AttributeFieldType = "email"
	AttributeFieldPhone        AttributeFieldType = "phone"
	AttributeFieldJSON         AttributeFieldType = "json"
)

// AttributeCardinality describes whether a definition has one or many values.
type AttributeCardinality string

const (
	AttributeCardinalitySingle AttributeCardinality = "single"
	AttributeCardinalityMulti  AttributeCardinality = "multi"
)

// AttributeOwnership distinguishes shipped definitions from user definitions.
type AttributeOwnership string

const (
	AttributeOwnershipSystem AttributeOwnership = "system"
	AttributeOwnershipUser   AttributeOwnership = "user"
)

var attributeObjectTypes = map[AttributeObjectType]bool{
	AttributeObjectPerson: true, AttributeObjectOrganization: true,
}

var attributeValueTypes = map[AttributeValueType]bool{
	AttributeValueText: true, AttributeValueInteger: true, AttributeValueReal: true,
	AttributeValueBoolean: true, AttributeValueDate: true, AttributeValueTimestamp: true,
	AttributeValueJSON: true, AttributeValueRecordReference: true,
}

var attributeFieldTypes = map[AttributeFieldType]bool{
	AttributeFieldText: true, AttributeFieldTextarea: true, AttributeFieldSelect: true,
	AttributeFieldMultiselect: true, AttributeFieldCheckbox: true, AttributeFieldDate: true,
	AttributeFieldTimestamp: true, AttributeFieldDuration: true, AttributeFieldPerson: true,
	AttributeFieldOrganization: true, AttributeFieldURL: true, AttributeFieldEmail: true,
	AttributeFieldPhone: true, AttributeFieldJSON: true,
}

var attributeRecordTargets = map[string]bool{"person": true}

var attributeSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

var attributeVCardPropertyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9-]{0,62}$`)

// AttributeChoice is one option offered by a select widget.
type AttributeChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// AttributeOptions is the closed settings envelope for a definition.
type AttributeOptions struct {
	Choices   []AttributeChoice `json:"choices,omitempty"`
	Unit      string            `json:"unit,omitempty"`
	MaxLength int               `json:"max_length,omitempty"`
}

// ChoiceValues returns option values in declaration order.
func (o *AttributeOptions) ChoiceValues() []string {
	if o == nil {
		return nil
	}
	values := make([]string, 0, len(o.Choices))
	for _, choice := range o.Choices {
		values = append(values, choice.Value)
	}
	return values
}

// AttributeDefinition is portable metadata for one typed field.
type AttributeDefinition struct {
	ID            int64                `json:"id"`
	UniversalID   string               `json:"universal_id"`
	ObjectType    AttributeObjectType  `json:"object_type"`
	Slug          string               `json:"slug"`
	Label         string               `json:"label"`
	Description   *string              `json:"description,omitempty"`
	ValueType     AttributeValueType   `json:"value_type"`
	FieldType     AttributeFieldType   `json:"field_type"`
	RecordTarget  *string              `json:"record_target,omitempty"`
	Cardinality   AttributeCardinality `json:"cardinality"`
	DisplayOrder  int64                `json:"display_order"`
	IsRequired    bool                 `json:"is_required"`
	Ownership     AttributeOwnership   `json:"ownership"`
	UICreatable   bool                 `json:"ui_creatable"`
	UIEditable    bool                 `json:"ui_editable"`
	APIMutable    bool                 `json:"api_mutable"`
	IsSearchable  bool                 `json:"is_searchable"`
	IsAudited     bool                 `json:"is_audited"`
	IsDeletable   bool                 `json:"is_deletable"`
	HistoryExempt bool                 `json:"history_exempt"`
	DerivedSource *string              `json:"derived_source,omitempty"`
	Options       *AttributeOptions    `json:"options,omitempty"`
	VCardProperty *string              `json:"vcard_property,omitempty"`
	IsActive      bool                 `json:"is_active"`
	Revision      int64                `json:"revision"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

// AttributeDefinitionInput is a complete new definition.
type AttributeDefinitionInput struct {
	UniversalID   string
	ObjectType    AttributeObjectType
	Slug          string
	Label         string
	Description   *string
	ValueType     AttributeValueType
	FieldType     AttributeFieldType
	RecordTarget  *string
	Cardinality   AttributeCardinality
	DisplayOrder  int64
	IsRequired    bool
	Ownership     AttributeOwnership
	UICreatable   bool
	UIEditable    bool
	APIMutable    bool
	IsSearchable  bool
	IsAudited     bool
	IsDeletable   bool
	HistoryExempt bool
	DerivedSource *string
	Options       *AttributeOptions
	VCardProperty *string
}

// AttributeDefinitionUpdate carries only mutable definition fields.
type AttributeDefinitionUpdate struct {
	Label        *string
	Description  **string
	DisplayOrder *int64
	IsActive     *bool
}

// AttributeDefinitionFilter narrows a definition listing.
type AttributeDefinitionFilter struct {
	ObjectType    AttributeObjectType
	IncludeHidden bool
}

// ValidateAttributeSlug checks the immutable machine-name syntax.
func ValidateAttributeSlug(slug string) error {
	if !attributeSlugPattern.MatchString(slug) {
		return fmt.Errorf(
			"%w: slug %q must match %s",
			ErrAttributeDefinitionInvalid, slug, attributeSlugPattern.String())
	}
	return nil
}

func validateAttributeDefinitionInput(
	input AttributeDefinitionInput,
) (AttributeDefinitionInput, error) {
	invalid := func(format string, args ...any) (AttributeDefinitionInput, error) {
		return AttributeDefinitionInput{}, fmt.Errorf(
			"%w: %s", ErrAttributeDefinitionInvalid, fmt.Sprintf(format, args...))
	}

	input.UniversalID = strings.TrimSpace(input.UniversalID)
	input.Slug = strings.TrimSpace(input.Slug)
	input.Label = strings.TrimSpace(input.Label)
	if input.Cardinality == "" {
		input.Cardinality = AttributeCardinalitySingle
	}
	if input.Ownership == "" {
		input.Ownership = AttributeOwnershipUser
	}

	if input.UniversalID == "" {
		return invalid("universal_id is required")
	}
	if err := ValidateAttributeSlug(input.Slug); err != nil {
		return AttributeDefinitionInput{}, err
	}
	if input.Label == "" {
		return invalid("label is required")
	}
	if !attributeObjectTypes[input.ObjectType] {
		return invalid("object_type %q is not supported", input.ObjectType)
	}
	if !attributeValueTypes[input.ValueType] {
		return invalid("value_type %q is not supported", input.ValueType)
	}
	if !attributeFieldTypes[input.FieldType] {
		return invalid("field_type %q is not supported", input.FieldType)
	}
	if input.Cardinality != AttributeCardinalitySingle &&
		input.Cardinality != AttributeCardinalityMulti {
		return invalid("cardinality %q is not supported", input.Cardinality)
	}
	if input.Ownership != AttributeOwnershipSystem &&
		input.Ownership != AttributeOwnershipUser {
		return invalid("ownership %q is not supported", input.Ownership)
	}
	if input.DisplayOrder < 0 {
		return invalid("display_order must not be negative")
	}

	if input.ValueType == AttributeValueRecordReference {
		if input.RecordTarget == nil || strings.TrimSpace(*input.RecordTarget) == "" {
			return invalid("record_target is required for value_type record_reference")
		}
		target := strings.TrimSpace(*input.RecordTarget)
		if !attributeRecordTargets[target] {
			return invalid("record_target %q is not supported yet", target)
		}
		input.RecordTarget = &target
	} else if input.RecordTarget != nil {
		return invalid("record_target is only valid for value_type record_reference")
	}

	if err := validateAttributeWidget(input); err != nil {
		return AttributeDefinitionInput{}, err
	}

	if input.VCardProperty != nil {
		property := strings.TrimSpace(*input.VCardProperty)
		if !attributeVCardPropertyPattern.MatchString(property) {
			return invalid("vcard_property %q must be an uppercase vCard token", property)
		}
		input.VCardProperty = &property
	}
	if input.DerivedSource != nil {
		derived := strings.TrimSpace(*input.DerivedSource)
		if derived == "" {
			return invalid("derived_source must not be blank when present")
		}
		input.APIMutable = false
		input.UICreatable = false
		input.UIEditable = false
		input.DerivedSource = &derived
	}
	if input.Description != nil {
		description := strings.TrimSpace(*input.Description)
		if description == "" {
			input.Description = nil
		} else {
			input.Description = &description
		}
	}
	return input, nil
}

func validateAttributeWidget(input AttributeDefinitionInput) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrAttributeDefinitionInvalid,
			fmt.Sprintf(format, args...))
	}
	switch input.FieldType {
	case AttributeFieldSelect, AttributeFieldMultiselect:
		if input.Options == nil || len(input.Options.Choices) == 0 {
			return invalid("field_type %s requires options.choices", input.FieldType)
		}
		if !attributeValueTypeHasCanonicalString(input.ValueType) {
			return invalid("field_type %s value_type %s has no canonical string form",
				input.FieldType, input.ValueType)
		}
	case AttributeFieldCheckbox:
		if input.ValueType != AttributeValueBoolean {
			return invalid("field_type checkbox requires value_type boolean")
		}
	case AttributeFieldDate:
		if input.ValueType != AttributeValueDate {
			return invalid("field_type date requires value_type date")
		}
	case AttributeFieldTimestamp:
		if input.ValueType != AttributeValueTimestamp {
			return invalid("field_type timestamp requires value_type timestamp")
		}
	case AttributeFieldDuration:
		if input.ValueType != AttributeValueInteger {
			return invalid("field_type duration requires value_type integer")
		}
	case AttributeFieldPerson, AttributeFieldOrganization:
		if input.ValueType != AttributeValueRecordReference {
			return invalid("field_type %s requires value_type record_reference",
				input.FieldType)
		}
		expectedTarget := string(input.FieldType)
		if input.RecordTarget == nil || *input.RecordTarget != expectedTarget {
			return invalid("field_type %s requires record_target %s",
				input.FieldType, expectedTarget)
		}
	case AttributeFieldJSON:
		if input.ValueType != AttributeValueJSON {
			return invalid("field_type json requires value_type json")
		}
	case AttributeFieldText, AttributeFieldTextarea:
		if input.ValueType != AttributeValueText &&
			input.ValueType != AttributeValueReal {
			return invalid("field_type %s requires value_type text or real", input.FieldType)
		}
	case AttributeFieldURL, AttributeFieldEmail, AttributeFieldPhone:
		if input.ValueType != AttributeValueText {
			return invalid("field_type %s requires value_type text", input.FieldType)
		}
	}
	if input.FieldType == AttributeFieldMultiselect &&
		input.Cardinality != AttributeCardinalityMulti {
		return invalid("field_type multiselect requires cardinality multi")
	}
	if input.Options != nil {
		seen := make(map[string]bool, len(input.Options.Choices))
		for i, choice := range input.Options.Choices {
			value, err := canonicalAttributeChoiceValue(
				input.ValueType, strings.TrimSpace(choice.Value))
			if err != nil {
				return invalid("options.choices[%d].value %q %s", i, choice.Value, err)
			}
			label := strings.TrimSpace(choice.Label)
			if value == "" {
				return invalid("options.choices[%d].value is required", i)
			}
			if label == "" {
				return invalid("options.choices[%d].label is required", i)
			}
			if seen[value] {
				return invalid("options.choices[%d].value %q is a duplicate", i, value)
			}
			seen[value] = true
			input.Options.Choices[i].Value = value
			input.Options.Choices[i].Label = label
		}
		if input.Options.MaxLength < 0 {
			return invalid("options.max_length must not be negative")
		}
	}
	return nil
}

func attributeValueTypeHasCanonicalString(valueType AttributeValueType) bool {
	switch valueType {
	case AttributeValueText, AttributeValueInteger, AttributeValueReal,
		AttributeValueBoolean, AttributeValueDate, AttributeValueTimestamp:
		return true
	default:
		return false
	}
}

func canonicalAttributeChoiceValue(
	valueType AttributeValueType, raw string,
) (string, error) {
	switch valueType {
	case AttributeValueText:
		return AttributeValue{
			Type: AttributeValueText,
			Text: &raw,
		}.CanonicalString()
	case AttributeValueInteger:
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return "", errors.New("must be an integer")
		}
		return AttributeValue{
			Type:    AttributeValueInteger,
			Integer: &value,
		}.CanonicalString()
	case AttributeValueReal:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", errors.New("must be a number")
		}
		return AttributeValue{
			Type: AttributeValueReal,
			Real: &value,
		}.CanonicalString()
	case AttributeValueBoolean:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return "", errors.New("must be a boolean")
		}
		return AttributeValue{
			Type:    AttributeValueBoolean,
			Boolean: &value,
		}.CanonicalString()
	case AttributeValueDate:
		value, err := time.Parse("2006-01-02", raw)
		if err != nil || value.Format("2006-01-02") != raw {
			return "", errors.New("must be a YYYY-MM-DD calendar date")
		}
		return AttributeValue{
			Type: AttributeValueDate,
			Date: &raw,
		}.CanonicalString()
	case AttributeValueTimestamp:
		value, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return "", errors.New("must be an RFC3339 timestamp")
		}
		return AttributeValue{
			Type:      AttributeValueTimestamp,
			Timestamp: &value,
		}.CanonicalString()
	default:
		return "", fmt.Errorf("uses value_type %s, which has no canonical string form",
			valueType)
	}
}

func marshalAttributeOptions(options *AttributeOptions) (sql.NullString, error) {
	if options == nil {
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return sql.NullString{}, fmt.Errorf(
			"%w: encode options: %w", ErrAttributeDefinitionInvalid, err)
	}
	if err := jsonexact.Validate(encoded, AttributeOptions{}); err != nil {
		return sql.NullString{}, fmt.Errorf(
			"%w: options: %w", ErrAttributeDefinitionInvalid, err)
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}

func unmarshalAttributeOptions(raw []byte) (*AttributeOptions, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var options AttributeOptions
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, false, fmt.Errorf("decode attribute options: %w", err)
	}
	return &options, true, nil
}

const attributeDefinitionColumns = `
	id, universal_id, object_type, slug, label, description,
	value_type, field_type, record_target, cardinality, display_order,
	is_required, ownership, ui_creatable, ui_editable, api_mutable,
	is_searchable, is_audited, is_deletable, history_exempt, derived_source,
	options, vcard_property, is_active, revision, created_at, updated_at
`

// CreateAttributeDefinitionContext validates and inserts a definition row.
func (s *Store) CreateAttributeDefinitionContext(
	ctx context.Context, input AttributeDefinitionInput,
) (*AttributeDefinition, error) {
	validated, err := validateAttributeDefinitionInput(input)
	if err != nil {
		return nil, err
	}
	options, err := marshalAttributeOptions(validated.Options)
	if err != nil {
		return nil, err
	}
	var definition *AttributeDefinition
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		query := fmt.Sprintf(`
			INSERT INTO attribute_definitions (
			    universal_id, object_type, slug, label, description,
			    value_type, field_type, record_target, cardinality, display_order,
			    is_required, ownership, ui_creatable, ui_editable, api_mutable,
			    is_searchable, is_audited, is_deletable, history_exempt,
			    derived_source, options, vcard_property
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s, ?)
			ON CONFLICT DO NOTHING
			RETURNING %s
		`, s.dialect.JSONBindExpr(), attributeDefinitionColumns)
		row := tx.QueryRowContext(ctx, query,
			validated.UniversalID, string(validated.ObjectType), validated.Slug,
			validated.Label, validated.Description, string(validated.ValueType),
			string(validated.FieldType), validated.RecordTarget,
			string(validated.Cardinality), validated.DisplayOrder,
			validated.IsRequired, string(validated.Ownership), validated.UICreatable,
			validated.UIEditable, validated.APIMutable, validated.IsSearchable,
			validated.IsAudited, validated.IsDeletable, validated.HistoryExempt,
			validated.DerivedSource, options, validated.VCardProperty)
		created, scanErr := scanAttributeDefinition(row)
		if scanErr != nil {
			return s.attributeDefinitionWriteError(ctx, tx, validated, scanErr)
		}
		definition = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return definition, nil
}

func (s *Store) attributeDefinitionWriteError(
	ctx context.Context, tx *loggedTx, input AttributeDefinitionInput, err error,
) error {
	if !errors.Is(err, sql.ErrNoRows) && !s.dialect.IsConflictError(err) {
		return fmt.Errorf("create attribute definition %q: %w", input.Slug, err)
	}
	var existing int
	if probeErr := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attribute_definitions WHERE universal_id = ?`,
		input.UniversalID,
	).Scan(&existing); probeErr != nil {
		return fmt.Errorf("classify attribute definition conflict: %w", probeErr)
	}
	if existing > 0 {
		return ErrAttributeDefinitionUniversalIDConflict
	}
	return ErrAttributeDefinitionSlugConflict
}

// GetAttributeDefinitionContext returns one definition by numeric ID.
func (s *Store) GetAttributeDefinitionContext(
	ctx context.Context, id int64,
) (*AttributeDefinition, error) {
	definition, err := scanAttributeDefinition(s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM attribute_definitions WHERE id = ?
	`, attributeDefinitionColumns), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAttributeDefinitionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get attribute definition %d: %w", id, err)
	}
	return definition, nil
}

// GetAttributeDefinitionBySlugContext returns a definition by scoped slug.
func (s *Store) GetAttributeDefinitionBySlugContext(
	ctx context.Context, objectType AttributeObjectType, slug string,
) (*AttributeDefinition, error) {
	definition, err := scanAttributeDefinition(s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM attribute_definitions WHERE object_type = ? AND slug = ?
	`, attributeDefinitionColumns), string(objectType), slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAttributeDefinitionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get attribute definition %s/%s: %w", objectType, slug, err)
	}
	return definition, nil
}

// ListAttributeDefinitionsContext lists definitions in display order.
func (s *Store) ListAttributeDefinitionsContext(
	ctx context.Context, filter AttributeDefinitionFilter,
) ([]AttributeDefinition, error) {
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if filter.ObjectType != "" {
		conditions = append(conditions, "object_type = ?")
		args = append(args, string(filter.ObjectType))
	}
	if !filter.IncludeHidden {
		conditions = append(conditions, s.dialect.BoolTrueExpr("is_active"))
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s FROM attribute_definitions
		%s
		ORDER BY object_type, display_order, slug, id
	`, attributeDefinitionColumns, where), args...)
	if err != nil {
		return nil, fmt.Errorf("list attribute definitions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	definitions := make([]AttributeDefinition, 0)
	for rows.Next() {
		definition, scanErr := scanAttributeDefinition(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan attribute definition: %w", scanErr)
		}
		definitions = append(definitions, *definition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attribute definitions: %w", err)
	}
	return definitions, nil
}

// UpdateAttributeDefinitionContext applies a revision-guarded mutable update.
func (s *Store) UpdateAttributeDefinitionContext(
	ctx context.Context, id, expectedRevision int64, update AttributeDefinitionUpdate,
) (*AttributeDefinition, error) {
	assignments := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if update.Label != nil {
		label := strings.TrimSpace(*update.Label)
		if label == "" {
			return nil, fmt.Errorf("%w: label must not be blank", ErrAttributeDefinitionInvalid)
		}
		assignments = append(assignments, "label = ?")
		args = append(args, label)
	}
	if update.Description != nil {
		description := *update.Description
		if description != nil {
			trimmed := strings.TrimSpace(*description)
			if trimmed == "" {
				description = nil
			} else {
				description = &trimmed
			}
		}
		assignments = append(assignments, "description = ?")
		args = append(args, description)
	}
	if update.DisplayOrder != nil {
		if *update.DisplayOrder < 0 {
			return nil, fmt.Errorf("%w: display_order must not be negative",
				ErrAttributeDefinitionInvalid)
		}
		assignments = append(assignments, "display_order = ?")
		args = append(args, *update.DisplayOrder)
	}
	if update.IsActive != nil {
		assignments = append(assignments, "is_active = ?")
		args = append(args, *update.IsActive)
	}
	if len(assignments) == 0 {
		return nil, fmt.Errorf("%w: no mutable field supplied", ErrAttributeDefinitionInvalid)
	}
	args = append(args, id, expectedRevision)

	var definition *AttributeDefinition
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		query := fmt.Sprintf(`
			UPDATE attribute_definitions
			SET %s, revision = revision + 1, updated_at = %s
			WHERE id = ? AND revision = ?
			RETURNING %s
		`, strings.Join(assignments, ", "), s.dialect.Now(), attributeDefinitionColumns)
		updated, scanErr := scanAttributeDefinition(tx.QueryRowContext(ctx, query, args...))
		if errors.Is(scanErr, sql.ErrNoRows) {
			return s.attributeDefinitionCASMissTx(ctx, tx, id)
		}
		if scanErr != nil {
			return fmt.Errorf("update attribute definition %d: %w", id, scanErr)
		}
		definition = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return definition, nil
}

// DeleteAttributeDefinitionContext removes an unused, user-owned definition.
func (s *Store) DeleteAttributeDefinitionContext(
	ctx context.Context, id, expectedRevision int64,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		definition, err := scanAttributeDefinition(tx.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT %s FROM attribute_definitions WHERE id = ?%s
		`, attributeDefinitionColumns, s.dialect.SelectForUpdate()), id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAttributeDefinitionNotFound
		}
		if err != nil {
			return fmt.Errorf("load attribute definition %d for delete: %w", id, err)
		}
		if definition.Revision != expectedRevision {
			return ErrAttributeDefinitionRevisionConflict
		}
		if !definition.IsDeletable || definition.Ownership == AttributeOwnershipSystem {
			return ErrAttributeDefinitionNotDeletable
		}
		var values int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM person_attribute_values WHERE definition_id = ?`, id,
		).Scan(&values); err != nil {
			return fmt.Errorf("count values for attribute definition %d: %w", id, err)
		}
		if values > 0 {
			return ErrAttributeDefinitionHasValues
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM attribute_definitions WHERE id = ? AND revision = ?`,
			id, expectedRevision,
		); err != nil {
			return fmt.Errorf("delete attribute definition %d: %w", id, err)
		}
		return nil
	})
}

func (s *Store) attributeDefinitionCASMissTx(
	ctx context.Context, tx *loggedTx, id int64,
) error {
	var revision int64
	err := tx.QueryRowContext(ctx,
		`SELECT revision FROM attribute_definitions WHERE id = ?`, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAttributeDefinitionNotFound
	}
	if err != nil {
		return fmt.Errorf("check attribute definition %d after revision miss: %w", id, err)
	}
	return ErrAttributeDefinitionRevisionConflict
}

func scanAttributeDefinition(row scanner) (*AttributeDefinition, error) {
	var (
		definition    AttributeDefinition
		description   sql.NullString
		recordTarget  sql.NullString
		derivedSource sql.NullString
		options       []byte
		vcardProperty sql.NullString
		objectType    string
		valueType     string
		fieldType     string
		cardinality   string
		ownership     string
	)
	if err := row.Scan(
		&definition.ID, &definition.UniversalID, &objectType, &definition.Slug,
		&definition.Label, &description, &valueType, &fieldType, &recordTarget,
		&cardinality, &definition.DisplayOrder, &definition.IsRequired, &ownership,
		&definition.UICreatable, &definition.UIEditable, &definition.APIMutable,
		&definition.IsSearchable, &definition.IsAudited, &definition.IsDeletable,
		&definition.HistoryExempt, &derivedSource, &options, &vcardProperty,
		&definition.IsActive, &definition.Revision, &definition.CreatedAt,
		&definition.UpdatedAt,
	); err != nil {
		return nil, err
	}
	definition.ObjectType = AttributeObjectType(objectType)
	definition.ValueType = AttributeValueType(valueType)
	definition.FieldType = AttributeFieldType(fieldType)
	definition.Cardinality = AttributeCardinality(cardinality)
	definition.Ownership = AttributeOwnership(ownership)
	if description.Valid {
		definition.Description = &description.String
	}
	if recordTarget.Valid {
		definition.RecordTarget = &recordTarget.String
	}
	if derivedSource.Valid {
		definition.DerivedSource = &derivedSource.String
	}
	if vcardProperty.Valid {
		definition.VCardProperty = &vcardProperty.String
	}
	parsed, present, err := unmarshalAttributeOptions(options)
	if err != nil {
		return nil, err
	}
	if present {
		definition.Options = parsed
	}
	return &definition, nil
}

// NewAttributeUniversalID mints a stable external definition identifier.
func NewAttributeUniversalID() (string, error) {
	uid, err := newVCardUID()
	if err != nil {
		return "", fmt.Errorf("generate attribute universal id: %w", err)
	}
	return uid, nil
}
