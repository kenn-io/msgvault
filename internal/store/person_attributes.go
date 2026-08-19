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
	SourceRef      *string        `json:"source_ref,omitempty"`
	Confidence     *float64       `json:"confidence,omitempty"`
	Actor          *string        `json:"actor,omitempty"`
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
	Value      *PersonAttributeValue `json:"value,omitempty"`
	Superseded *PersonAttributeValue `json:"superseded,omitempty"`
	DryRun     bool                  `json:"dry_run"`
}

// PersonAttributeQuery filters a person's attribute values.
type PersonAttributeQuery struct {
	DefinitionSlug string
	IncludeHistory bool
}

const personAttributeValueColumns = `
	v.id, v.person_id, v.definition_id, d.slug, v.ordinal,
	d.value_type, v.value_text, v.value_integer, v.value_real, v.value_boolean,
	v.value_date, v.value_timestamp, v.value_json, v.value_record_type,
	v.value_record_id, v.active_from, v.active_until, v.created_at,
	v.superseded_at, v.source, v.source_ref, v.confidence, v.actor
`

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
	conditions := []string{"v.person_id = ?"}
	args := []any{personID}
	if query.DefinitionSlug != "" {
		conditions = append(conditions, "d.slug = ?", "d.object_type = ?")
		args = append(args, query.DefinitionSlug, string(AttributeObjectPerson))
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
	rows, err := queryer.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE %s
		ORDER BY %s
	`, personAttributeValueColumns, strings.Join(conditions, " AND "), order), args...)
	if err != nil {
		return nil, fmt.Errorf("list person %d attribute values: %w", personID, err)
	}
	defer func() { _ = rows.Close() }()

	values := make([]PersonAttributeValue, 0)
	for rows.Next() {
		value, scanErr := scanPersonAttributeValue(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person attribute value: %w", scanErr)
		}
		values = append(values, *value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person attribute values: %w", err)
	}
	return values, nil
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
	if input.Ordinal != nil && *input.Ordinal < 0 {
		return nil, fmt.Errorf("%w: ordinal must not be negative", ErrAttributeValueInvalid)
	}
	return retryContendedWrite(ctx, s, "set person attribute value",
		func() (*PersonAttributeWrite, error) {
			return s.setPersonAttributeValueOnce(ctx, input)
		})
}

func (s *Store) setPersonAttributeValueOnce(
	ctx context.Context, input PersonAttributeValueInput,
) (*PersonAttributeWrite, error) {
	activeFrom := time.Now().UTC()
	if input.ActiveFrom != nil {
		activeFrom = input.ActiveFrom.UTC()
	}
	if input.ActiveUntil != nil && input.ActiveUntil.Before(activeFrom) {
		return nil, fmt.Errorf("%w: active_until must not precede active_from",
			ErrAttributeValueInvalid)
	}

	write := &PersonAttributeWrite{DryRun: input.DryRun}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		loaded, err := s.getAttributeDefinitionBySlugTx(
			ctx, tx, AttributeObjectPerson, input.DefinitionSlug)
		if err != nil {
			return err
		}
		definition := *loaded
		if err := writableAttributeDefinition(definition); err != nil {
			return err
		}
		value, err := normalizeAttributeValue(definition, input.Value)
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
		var personExists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM persons WHERE id = ?`, input.PersonID,
		).Scan(&personExists); err != nil {
			return fmt.Errorf("verify person %d: %w", input.PersonID, err)
		}
		if personExists == 0 {
			return ErrPersonNotFound
		}
		if err := s.verifyAttributeRecordTargetTx(ctx, tx, input.Value); err != nil {
			return err
		}
		ordinal, err := s.resolveAttributeOrdinalTx(ctx, tx, definition, input)
		if err != nil {
			return err
		}
		current, hasCurrent, err := s.currentPersonAttributeValueTx(
			ctx, tx, input.PersonID, definition.ID, ordinal)
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
			closed, err := s.closePersonAttributeValueTx(
				ctx, tx, current.ID, activeFrom, time.Now().UTC())
			if err != nil {
				return err
			}
			write.Superseded = closed
		}
		inserted, err := s.insertPersonAttributeValueTx(
			ctx, tx, definition, input, ordinal, activeFrom)
		if err != nil {
			return err
		}
		write.Value = inserted
		if err := s.bumpPersonVCardProjectionsTx(
			ctx, tx, input.PersonID,
		); err != nil {
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

func (s *Store) resolveAttributeOrdinalTx(
	ctx context.Context, tx *loggedTx,
	definition AttributeDefinition, input PersonAttributeValueInput,
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
		FROM person_attribute_values
		WHERE person_id = ? AND definition_id = ?
	`, input.PersonID, definition.ID).Scan(&next); err != nil {
		return 0, fmt.Errorf("resolve next ordinal for %s: %w", definition.Slug, err)
	}
	return next, nil
}

func (s *Store) verifyAttributeRecordTargetTx(
	ctx context.Context, tx *loggedTx, value AttributeValue,
) error {
	if value.Type != AttributeValueRecordReference {
		return nil
	}
	switch *value.RecordType {
	case "person":
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM persons WHERE id = ?`, *value.RecordID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("verify referenced person %d: %w", *value.RecordID, err)
		}
		if exists == 0 {
			return fmt.Errorf("%w: referenced person %d does not exist",
				ErrAttributeValueInvalid, *value.RecordID)
		}
		return nil
	default:
		return fmt.Errorf("%w: record_type %q is not supported yet",
			ErrAttributeValueInvalid, *value.RecordType)
	}
}

func (s *Store) currentPersonAttributeValueTx(
	ctx context.Context, tx *loggedTx, personID, definitionID, ordinal int64,
) (*PersonAttributeValue, bool, error) {
	value, err := scanPersonAttributeValue(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.person_id = ? AND v.definition_id = ? AND v.ordinal = ?
		  AND v.active_until IS NULL AND v.superseded_at IS NULL%s
	`, personAttributeValueColumns, s.dialect.SelectForUpdate()),
		personID, definitionID, ordinal))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load current attribute value: %w", err)
	}
	return value, true, nil
}

func (s *Store) closePersonAttributeValueTx(
	ctx context.Context, tx *loggedTx, valueID int64,
	activeUntil, supersededAt time.Time,
) (*PersonAttributeValue, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE person_attribute_values
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
	closed, err := scanPersonAttributeValue(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.id = ?
	`, personAttributeValueColumns), valueID))
	if err != nil {
		return nil, fmt.Errorf("re-read closed attribute value %d: %w", valueID, err)
	}
	return closed, nil
}

func (s *Store) insertPersonAttributeValueTx(
	ctx context.Context, tx *loggedTx, definition AttributeDefinition,
	input PersonAttributeValueInput, ordinal int64, activeFrom time.Time,
) (*PersonAttributeValue, error) {
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
		INSERT INTO person_attribute_values (
		    person_id, definition_id, ordinal,
		    value_text, value_integer, value_real, value_boolean,
		    value_date, value_timestamp, value_json,
		    value_record_type, value_record_id,
		    active_from, active_until, source, source_ref, confidence, actor
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, %s, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id
	`, s.dialect.JSONBindExpr()),
		input.PersonID, definition.ID, ordinal,
		input.Value.Text, input.Value.Integer, input.Value.Real, input.Value.Boolean,
		input.Value.Date, input.Value.Timestamp, jsonValue,
		input.Value.RecordType, input.Value.RecordID,
		activeFrom, activeUntil, string(input.Source), input.SourceRef,
		input.Confidence, input.Actor,
	).Scan(&insertedID); err != nil {
		return nil, fmt.Errorf("insert attribute value for %s: %w", definition.Slug, err)
	}
	inserted, err := scanPersonAttributeValue(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.id = ?
	`, personAttributeValueColumns), insertedID))
	if err != nil {
		return nil, fmt.Errorf("re-read inserted attribute value: %w", err)
	}
	return inserted, nil
}

// SupersedePersonAttributeValueContext closes a current value without
// replacement. Unlike Set it also works on inactive definitions: retracting a
// stale value from a retired definition must stay possible. As in Set, the
// definition is loaded and checked inside each attempt's transaction.
func (s *Store) SupersedePersonAttributeValueContext(
	ctx context.Context, input PersonAttributeSupersedeInput,
) (*PersonAttributeWrite, error) {
	if input.Ordinal != nil && *input.Ordinal < 0 {
		return nil, fmt.Errorf("%w: ordinal must not be negative", ErrAttributeValueInvalid)
	}
	at := time.Now().UTC()
	if input.At != nil {
		at = input.At.UTC()
	}
	return retryContendedWrite(ctx, s, "supersede person attribute value",
		func() (*PersonAttributeWrite, error) {
			return s.supersedePersonAttributeValueOnce(ctx, at, input)
		})
}

func (s *Store) supersedePersonAttributeValueOnce(
	ctx context.Context, at time.Time, input PersonAttributeSupersedeInput,
) (*PersonAttributeWrite, error) {
	write := &PersonAttributeWrite{DryRun: input.DryRun}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		definition, err := s.getAttributeDefinitionBySlugTx(
			ctx, tx, AttributeObjectPerson, input.DefinitionSlug)
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
		current, hasCurrent, err := s.currentPersonAttributeValueTx(
			ctx, tx, input.PersonID, definition.ID, ordinal)
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
		closed, err := s.closePersonAttributeValueTx(
			ctx, tx, current.ID, at, time.Now().UTC())
		if err != nil {
			return err
		}
		write.Superseded = closed
		if err := s.bumpPersonVCardProjectionsTx(
			ctx, tx, input.PersonID,
		); err != nil {
			return err
		}
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

func scanPersonAttributeValue(row scanner) (*PersonAttributeValue, error) {
	var (
		value        PersonAttributeValue
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
		&value.ID, &value.PersonID, &value.DefinitionID, &value.DefinitionSlug,
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
