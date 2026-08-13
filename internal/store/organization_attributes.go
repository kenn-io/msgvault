package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	SourceRef      *string        `json:"source_ref,omitempty"`
	Confidence     *float64       `json:"confidence,omitempty"`
	Actor          *string        `json:"actor,omitempty"`
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
	Value      *OrganizationAttributeValue `json:"value,omitempty"`
	Superseded *OrganizationAttributeValue `json:"superseded,omitempty"`
	DryRun     bool                        `json:"dry_run"`
}

// OrganizationAttributeQuery filters an organization's attribute values.
type OrganizationAttributeQuery struct {
	DefinitionSlug string
	IncludeHistory bool
}

const organizationAttributeValueColumns = `
	v.id, v.organization_id, v.definition_id, d.slug, v.ordinal,
	d.value_type, v.value_text, v.value_integer, v.value_real, v.value_boolean,
	v.value_date, v.value_timestamp, v.value_json, v.value_record_type,
	v.value_record_id, v.active_from, v.active_until, v.created_at,
	v.superseded_at, v.source, v.source_ref, v.confidence, v.actor
`

// ListOrganizationAttributeValuesContext lists current or historical values.
func (s *Store) ListOrganizationAttributeValuesContext(
	ctx context.Context, organizationID int64, query OrganizationAttributeQuery,
) ([]OrganizationAttributeValue, error) {
	conditions := []string{"v.organization_id = ?"}
	args := []any{organizationID}
	if query.DefinitionSlug != "" {
		conditions = append(conditions, "d.slug = ?", "d.object_type = ?")
		args = append(args, query.DefinitionSlug, string(AttributeObjectOrganization))
	}
	if !query.IncludeHistory {
		conditions = append(conditions,
			"v.active_until IS NULL", "v.superseded_at IS NULL")
	}
	order := "d.display_order, d.slug, v.ordinal, v.id"
	if query.IncludeHistory {
		order = "d.display_order, d.slug, v.ordinal, " +
			"CASE WHEN v.active_until IS NULL AND v.superseded_at IS NULL " +
			"THEN 0 ELSE 1 END, v.active_from DESC, v.id DESC"
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM organization_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE %s
		ORDER BY %s
	`, organizationAttributeValueColumns, strings.Join(conditions, " AND "), order), args...)
	if err != nil {
		return nil, fmt.Errorf("list organization %d attribute values: %w", organizationID, err)
	}
	defer func() { _ = rows.Close() }()

	values := make([]OrganizationAttributeValue, 0)
	for rows.Next() {
		value, scanErr := scanOrganizationAttributeValue(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan organization attribute value: %w", scanErr)
		}
		values = append(values, *value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate organization attribute values: %w", err)
	}
	return values, nil
}

// SetOrganizationAttributeValueContext supersedes the current value and inserts a replacement.
func (s *Store) SetOrganizationAttributeValueContext(
	ctx context.Context, input OrganizationAttributeValueInput,
) (*OrganizationAttributeWrite, error) {
	if err := validateProvenance(input.Source, input.Confidence); err != nil {
		return nil, err
	}
	if input.Ordinal != nil && *input.Ordinal < 0 {
		return nil, fmt.Errorf("%w: ordinal must not be negative", ErrAttributeValueInvalid)
	}

	return retryContendedWrite(s, "set organization attribute value",
		func() (*OrganizationAttributeWrite, error) {
			return s.setOrganizationAttributeValueOnce(ctx, input)
		})
}

func (s *Store) setOrganizationAttributeValueOnce(
	ctx context.Context, input OrganizationAttributeValueInput,
) (*OrganizationAttributeWrite, error) {
	writeTime := time.Now().UTC()
	activeFrom := writeTime
	if input.ActiveFrom != nil {
		activeFrom = input.ActiveFrom.UTC()
	}
	if input.ActiveUntil != nil && input.ActiveUntil.Before(activeFrom) {
		return nil, fmt.Errorf("%w: active_until must not precede active_from",
			ErrAttributeValueInvalid)
	}

	write := &OrganizationAttributeWrite{DryRun: input.DryRun}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		definition, err := s.getOrganizationAttributeDefinitionTx(
			ctx, tx, input.DefinitionSlug)
		if err != nil {
			return err
		}
		if err := writableAttributeDefinition(*definition); err != nil {
			return err
		}
		value, err := normalizeAttributeValue(*definition, input.Value)
		if err != nil {
			return err
		}
		input.Value = value
		if definition.Cardinality == AttributeCardinalitySingle &&
			input.Ordinal != nil && *input.Ordinal != 0 {
			return fmt.Errorf(
				"%w: ordinal %d is not allowed on %s, which declares cardinality single",
				ErrAttributeValueInvalid, *input.Ordinal, definition.Slug)
		}
		if input.Value.Type == AttributeValueRecordReference {
			if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
				return err
			}
		}
		organization, err := getOrganizationForUpdateTx(
			ctx, tx, s.dialect, input.OrganizationID)
		if err != nil {
			return err
		}
		if organization.MergedIntoID != nil {
			return fmt.Errorf("%w: merged organization redirects are immutable",
				ErrOrganizationInvalid)
		}
		if err := s.verifyAttributeRecordTargetTx(ctx, tx, input.Value); err != nil {
			return err
		}
		ordinal, err := s.resolveOrganizationAttributeOrdinalTx(ctx, tx, *definition, input)
		if err != nil {
			return err
		}
		current, hasCurrent, err := s.currentOrganizationAttributeValueTx(
			ctx, tx, input.OrganizationID, definition.ID, ordinal)
		if err != nil {
			return err
		}
		if input.ExpectedValueID != nil &&
			(!hasCurrent || current.ID != *input.ExpectedValueID) {
			return ErrAttributeValueConflict
		}
		if hasCurrent {
			if activeFrom.Before(current.ActiveFrom) {
				return fmt.Errorf("%w: active_from precedes the current value",
					ErrAttributeValueInvalid)
			}
			closed, err := s.closeOrganizationAttributeValueTx(
				ctx, tx, current.ID, activeFrom, writeTime)
			if err != nil {
				return err
			}
			write.Superseded = closed
		}
		inserted, err := s.insertOrganizationAttributeValueTx(
			ctx, tx, *definition, input, ordinal, activeFrom)
		if err != nil {
			return err
		}
		write.Value = inserted
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

func (s *Store) resolveOrganizationAttributeOrdinalTx(
	ctx context.Context, tx *loggedTx,
	definition AttributeDefinition, input OrganizationAttributeValueInput,
) (int64, error) {
	if definition.Cardinality == AttributeCardinalitySingle {
		return 0, nil
	}
	if input.Ordinal != nil {
		return *input.Ordinal, nil
	}
	// Scan historical rows too: reusing a superseded slot's ordinal would
	// splice an unrelated value into that slot's supersession lineage.
	var next int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(ordinal) + 1, 0)
		FROM organization_attribute_values
		WHERE organization_id = ? AND definition_id = ?
	`, input.OrganizationID, definition.ID).Scan(&next); err != nil {
		return 0, fmt.Errorf("resolve next ordinal for %s: %w", definition.Slug, err)
	}
	return next, nil
}

func (s *Store) currentOrganizationAttributeValueTx(
	ctx context.Context, tx *loggedTx, organizationID, definitionID, ordinal int64,
) (*OrganizationAttributeValue, bool, error) {
	value, err := scanOrganizationAttributeValue(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM organization_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.organization_id = ? AND v.definition_id = ? AND v.ordinal = ?
		  AND v.active_until IS NULL AND v.superseded_at IS NULL%s
	`, organizationAttributeValueColumns, s.dialect.SelectForUpdate()),
		organizationID, definitionID, ordinal))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load current attribute value: %w", err)
	}
	return value, true, nil
}

func (s *Store) closeOrganizationAttributeValueTx(
	ctx context.Context, tx *loggedTx, valueID int64,
	activeUntil, supersededAt time.Time,
) (*OrganizationAttributeValue, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE organization_attribute_values
		SET active_until = ?, superseded_at = ?
		WHERE id = ? AND active_until IS NULL AND superseded_at IS NULL
	`, activeUntil, supersededAt, valueID)
	if err != nil {
		return nil, fmt.Errorf("close attribute value %d: %w", valueID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check close of attribute value %d: %w", valueID, err)
	}
	if affected != 1 {
		return nil, ErrAttributeValueConflict
	}
	closed, err := scanOrganizationAttributeValue(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM organization_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.id = ?
	`, organizationAttributeValueColumns), valueID))
	if err != nil {
		return nil, fmt.Errorf("re-read closed attribute value %d: %w", valueID, err)
	}
	return closed, nil
}

func (s *Store) insertOrganizationAttributeValueTx(
	ctx context.Context, tx *loggedTx, definition AttributeDefinition,
	input OrganizationAttributeValueInput, ordinal int64, activeFrom time.Time,
) (*OrganizationAttributeValue, error) {
	var jsonValue any
	if len(input.Value.JSON) > 0 {
		jsonValue = string(input.Value.JSON)
	}
	var activeUntil any
	if input.ActiveUntil != nil {
		activeUntil = input.ActiveUntil.UTC()
	}
	var insertedID int64
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		INSERT INTO organization_attribute_values (
		    organization_id, definition_id, ordinal,
		    value_text, value_integer, value_real, value_boolean,
		    value_date, value_timestamp, value_json,
		    value_record_type, value_record_id,
		    active_from, active_until, source, source_ref, confidence, actor
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, %s, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id
	`, s.dialect.JSONBindExpr()),
		input.OrganizationID, definition.ID, ordinal,
		input.Value.Text, input.Value.Integer, input.Value.Real, input.Value.Boolean,
		input.Value.Date, input.Value.Timestamp, jsonValue,
		input.Value.RecordType, input.Value.RecordID,
		activeFrom, activeUntil, string(input.Source), input.SourceRef,
		input.Confidence, input.Actor,
	).Scan(&insertedID); err != nil {
		return nil, fmt.Errorf("insert attribute value for %s: %w", definition.Slug, err)
	}
	inserted, err := scanOrganizationAttributeValue(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM organization_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.id = ?
	`, organizationAttributeValueColumns), insertedID))
	if err != nil {
		return nil, fmt.Errorf("re-read inserted attribute value: %w", err)
	}
	return inserted, nil
}

// SupersedeOrganizationAttributeValueContext closes a current value without replacement.
func (s *Store) SupersedeOrganizationAttributeValueContext(
	ctx context.Context, input OrganizationAttributeSupersedeInput,
) (*OrganizationAttributeWrite, error) {
	if input.Ordinal != nil && *input.Ordinal < 0 {
		return nil, fmt.Errorf("%w: ordinal must not be negative", ErrAttributeValueInvalid)
	}
	at := time.Now().UTC()
	if input.At != nil {
		at = input.At.UTC()
	}

	return retryContendedWrite(s, "supersede organization attribute value",
		func() (*OrganizationAttributeWrite, error) {
			return s.supersedeOrganizationAttributeValueOnce(ctx, at, input)
		})
}

func (s *Store) supersedeOrganizationAttributeValueOnce(
	ctx context.Context, at time.Time, input OrganizationAttributeSupersedeInput,
) (*OrganizationAttributeWrite, error) {
	writeTime := time.Now().UTC()
	write := &OrganizationAttributeWrite{DryRun: input.DryRun}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		definition, err := s.getOrganizationAttributeDefinitionTx(
			ctx, tx, input.DefinitionSlug)
		if err != nil {
			return err
		}
		if err := retractableAttributeDefinition(*definition); err != nil {
			return err
		}
		ordinal := int64(0)
		if input.Ordinal != nil {
			if definition.Cardinality == AttributeCardinalitySingle && *input.Ordinal != 0 {
				return fmt.Errorf(
					"%w: ordinal %d is not allowed on %s, which declares cardinality single",
					ErrAttributeValueInvalid, *input.Ordinal, definition.Slug)
			}
			ordinal = *input.Ordinal
		}
		organization, err := getOrganizationForUpdateTx(
			ctx, tx, s.dialect, input.OrganizationID)
		if err != nil {
			return err
		}
		if organization.MergedIntoID != nil {
			return fmt.Errorf("%w: merged organization redirects are immutable",
				ErrOrganizationInvalid)
		}
		current, hasCurrent, err := s.currentOrganizationAttributeValueTx(
			ctx, tx, input.OrganizationID, definition.ID, ordinal)
		if err != nil {
			return err
		}
		if !hasCurrent {
			return ErrAttributeValueNotFound
		}
		if input.ExpectedValueID != nil && current.ID != *input.ExpectedValueID {
			return ErrAttributeValueConflict
		}
		if at.Before(current.ActiveFrom) {
			return fmt.Errorf("%w: supersede time precedes active_from",
				ErrAttributeValueInvalid)
		}
		closed, err := s.closeOrganizationAttributeValueTx(
			ctx, tx, current.ID, at, writeTime)
		if err != nil {
			return err
		}
		write.Superseded = closed
		if input.DryRun {
			return errAttributeDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errAttributeDryRun) {
		return nil, err
	}
	return write, nil
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

func scanOrganizationAttributeValue(row scanner) (*OrganizationAttributeValue, error) {
	var (
		value        OrganizationAttributeValue
		valueType    string
		text         sql.NullString
		integer      sql.NullInt64
		realValue    sql.NullFloat64
		boolean      sql.NullBool
		date         sql.NullString
		timestamp    sql.NullTime
		rawJSON      []byte
		recordType   sql.NullString
		recordID     sql.NullInt64
		activeUntil  sql.NullTime
		supersededAt sql.NullTime
		source       string
		sourceRef    sql.NullString
		confidence   sql.NullFloat64
		actor        sql.NullString
	)
	if err := row.Scan(
		&value.ID, &value.OrganizationID, &value.DefinitionID, &value.DefinitionSlug,
		&value.Ordinal, &valueType, &text, &integer, &realValue, &boolean, &date,
		&timestamp, &rawJSON, &recordType, &recordID, &value.ActiveFrom,
		&activeUntil, &value.CreatedAt, &supersededAt, &source, &sourceRef,
		&confidence, &actor,
	); err != nil {
		return nil, err
	}
	value.Value.Type = AttributeValueType(valueType)
	if text.Valid {
		value.Value.Text = &text.String
	}
	if integer.Valid {
		value.Value.Integer = &integer.Int64
	}
	if realValue.Valid {
		value.Value.Real = &realValue.Float64
	}
	if boolean.Valid {
		value.Value.Boolean = &boolean.Bool
	}
	if date.Valid {
		value.Value.Date = &date.String
	}
	if timestamp.Valid {
		utc := timestamp.Time.UTC()
		value.Value.Timestamp = &utc
	}
	if len(rawJSON) > 0 {
		value.Value.JSON = json.RawMessage(append([]byte(nil), rawJSON...))
	}
	if recordType.Valid {
		value.Value.RecordType = &recordType.String
	}
	if recordID.Valid {
		value.Value.RecordID = &recordID.Int64
	}
	if activeUntil.Valid {
		utc := activeUntil.Time.UTC()
		value.ActiveUntil = &utc
	}
	if supersededAt.Valid {
		utc := supersededAt.Time.UTC()
		value.SupersededAt = &utc
	}
	value.Source = Provenance(source)
	if sourceRef.Valid {
		value.SourceRef = &sourceRef.String
	}
	if confidence.Valid {
		value.Confidence = &confidence.Float64
	}
	if actor.Valid {
		value.Actor = &actor.String
	}
	value.ActiveFrom = value.ActiveFrom.UTC()
	value.CreatedAt = value.CreatedAt.UTC()
	return &value, nil
}
