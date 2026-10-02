package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"time"
)

// attributeOwner names one subject's value table and the checks that differ
// between subjects. Person and organization values share every row rule.
type attributeOwner struct {
	objectType AttributeObjectType
	noun       string // used in list error text
	table      string
	column     string
	// stampCreatedAt binds the transaction time; otherwise the column default applies.
	stampCreatedAt bool
	// lockOwner runs after the definition checks; set is false for a supersede.
	lockOwner func(ctx context.Context, s *Store, tx *loggedTx, ownerID int64, set bool) error
	// conflict builds the error for a stale expected value; current may be nil.
	conflict func(current *attributeValueRow) error
}

// attributeValueRow is one value row of either owner table.
type attributeValueRow struct {
	ID             int64
	OwnerID        int64
	DefinitionID   int64
	DefinitionSlug string
	Ordinal        int64
	Value          AttributeValue
	ActiveFrom     time.Time
	ActiveUntil    *time.Time
	CreatedAt      time.Time
	SupersededAt   *time.Time
	Source         Provenance
	SourceRef      *string
	Confidence     *float64
	Actor          *string
}

type attributeValueWrite struct {
	Value      *attributeValueRow
	Superseded *attributeValueRow
	DryRun     bool
}

type attributeValueInput struct {
	OwnerID         int64
	Ordinal         *int64
	Value           AttributeValue
	ActiveUntil     *time.Time
	Source          Provenance
	SourceRef       *string
	Confidence      *float64
	Actor           *string
	ExpectedValueID *int64
}

func (o attributeOwner) columns() string {
	return `
	v.id, v.` + o.column + `, v.definition_id, d.slug, v.ordinal,
	d.value_type, v.value_text, v.value_integer, v.value_real, v.value_boolean,
	v.value_date, v.value_timestamp, v.value_json, v.value_record_type,
	v.value_record_id, v.active_from, v.active_until, v.created_at,
	v.superseded_at, v.source, v.source_ref, v.confidence, v.actor
`
}

func (r *attributeValueRow) person() *PersonAttributeValue {
	if r == nil {
		return nil
	}
	return &PersonAttributeValue{
		ID: r.ID, PersonID: r.OwnerID, DefinitionID: r.DefinitionID,
		DefinitionSlug: r.DefinitionSlug, Ordinal: r.Ordinal, Value: r.Value,
		ActiveFrom: r.ActiveFrom, ActiveUntil: r.ActiveUntil, CreatedAt: r.CreatedAt,
		SupersededAt: r.SupersededAt, Source: r.Source, SourceRef: r.SourceRef,
		Confidence: r.Confidence, Actor: r.Actor,
	}
}

func (r *attributeValueRow) organization() *OrganizationAttributeValue {
	if r == nil {
		return nil
	}
	return &OrganizationAttributeValue{
		ID: r.ID, OrganizationID: r.OwnerID, DefinitionID: r.DefinitionID,
		DefinitionSlug: r.DefinitionSlug, Ordinal: r.Ordinal, Value: r.Value,
		ActiveFrom: r.ActiveFrom, ActiveUntil: r.ActiveUntil, CreatedAt: r.CreatedAt,
		SupersededAt: r.SupersededAt, Source: r.Source, SourceRef: r.SourceRef,
		Confidence: r.Confidence, Actor: r.Actor,
	}
}

func (w *attributeValueWrite) person() *PersonAttributeWrite {
	if w == nil {
		return nil
	}
	return &PersonAttributeWrite{
		Value: w.Value.person(), Superseded: w.Superseded.person(), DryRun: w.DryRun,
	}
}

func (w *attributeValueWrite) organization() *OrganizationAttributeWrite {
	if w == nil {
		return nil
	}
	return &OrganizationAttributeWrite{
		Value: w.Value.organization(), Superseded: w.Superseded.organization(), DryRun: w.DryRun,
	}
}

func attributeRowsAs[T any](rows []attributeValueRow, as func(*attributeValueRow) *T) []T {
	values := make([]T, 0, len(rows))
	for i := range rows {
		values = append(values, *as(&rows[i]))
	}
	return values
}

func validateAttributeOrdinal(ordinal *int64) error {
	if ordinal != nil && *ordinal < 0 {
		return fmt.Errorf("%w: ordinal must not be negative", ErrAttributeValueInvalid)
	}
	return nil
}

func attributeActiveFrom(activeFrom, activeUntil *time.Time, now time.Time) (time.Time, error) {
	from := now
	if activeFrom != nil {
		from = activeFrom.UTC()
	}
	if activeUntil != nil && activeUntil.Before(from) {
		return time.Time{}, fmt.Errorf("%w: active_until must not precede active_from",
			ErrAttributeValueInvalid)
	}
	return from, nil
}

// runAttributeWrite retries one write attempt in its own transaction. Each
// attempt reads a fresh clock, and a dry run rolls back after the full write.
func (s *Store) runAttributeWrite(
	ctx context.Context, operation string, dryRun bool,
	write func(tx *loggedTx, now time.Time) (*attributeValueWrite, error),
) (*attributeValueWrite, error) {
	return retryContendedWrite(ctx, s, operation, func() (*attributeValueWrite, error) {
		var result *attributeValueWrite
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			var err error
			result, err = write(tx, time.Now().UTC())
			if err != nil {
				return err
			}
			result.DryRun = dryRun
			if dryRun {
				if result.Value != nil {
					result.Value.ID = 0
				}
				return errAttributeDryRun
			}
			return nil
		})
		if err != nil && !errors.Is(err, errAttributeDryRun) {
			return nil, err
		}
		return result, nil
	})
}

func (s *Store) listAttributeValuesContext(
	ctx context.Context, queryer contextRowsQuerier, owner attributeOwner,
	ownerID int64, definitionSlug string, includeHistory bool,
) ([]attributeValueRow, error) {
	conditions := []string{"v." + owner.column + " = ?"}
	args := []any{ownerID}
	if definitionSlug != "" {
		conditions = append(conditions, "d.slug = ?", "d.object_type = ?")
		args = append(args, definitionSlug, string(owner.objectType))
	}
	if !includeHistory {
		conditions = append(conditions,
			"v.active_until IS NULL", "v.superseded_at IS NULL")
	}
	order := "d.display_order, d.slug, v.ordinal, v.id"
	if includeHistory {
		order = "d.display_order, d.slug, v.ordinal, " +
			"CASE WHEN v.active_until IS NULL AND v.superseded_at IS NULL " +
			"THEN 0 ELSE 1 END, v.active_from DESC, v.id DESC"
	}
	rows, err := queryer.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %s v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE %s
		ORDER BY %s
	`, owner.columns(), owner.table, strings.Join(conditions, " AND "), order), args...)
	if err != nil {
		return nil, fmt.Errorf("list %s %d attribute values: %w", owner.noun, ownerID, err)
	}
	defer func() { _ = rows.Close() }()

	values := make([]attributeValueRow, 0)
	for rows.Next() {
		value, scanErr := scanAttributeValueRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan %s attribute value: %w", owner.noun, scanErr)
		}
		values = append(values, *value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s attribute values: %w", owner.noun, err)
	}
	return values, nil
}

// setAttributeValueTx supersedes the current value in the slot and inserts a
// replacement. The caller has loaded the definition and taken its own locks.
func (s *Store) setAttributeValueTx(
	ctx context.Context, tx *loggedTx, owner attributeOwner, definition AttributeDefinition,
	input attributeValueInput, activeFrom, transactionTime time.Time,
) (*attributeValueWrite, error) {
	if err := writableAttributeDefinition(definition); err != nil {
		return nil, err
	}
	value, err := normalizeAttributeValue(definition, input.Value)
	if err != nil {
		return nil, err
	}
	input.Value = value
	if definition.Cardinality == AttributeCardinalitySingle &&
		input.Ordinal != nil && *input.Ordinal != 0 {
		return nil, fmt.Errorf(
			"%w: ordinal %d is not allowed on %s, which declares cardinality single",
			ErrAttributeValueInvalid, *input.Ordinal, definition.Slug)
	}
	if input.Value.Type == AttributeValueRecordReference {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return nil, err
		}
	}
	if err := owner.lockOwner(ctx, s, tx, input.OwnerID, true); err != nil {
		return nil, err
	}
	if err := s.verifyAttributeRecordTargetTx(ctx, tx, input.Value); err != nil {
		return nil, err
	}
	var ordinal int64
	switch {
	case definition.Cardinality == AttributeCardinalitySingle:
	case input.Ordinal != nil:
		ordinal = *input.Ordinal
	default:
		next, err := nextProfileOrdinalForOwnerTx(
			ctx, tx, owner.table, owner.column, "definition_id", input.OwnerID, definition.ID)
		if err != nil {
			return nil, err
		}
		ordinal = int64(next)
	}
	current, hasCurrent, err := s.currentAttributeValueTx(
		ctx, tx, owner, input.OwnerID, definition.ID, ordinal)
	if err != nil {
		return nil, err
	}
	if input.ExpectedValueID != nil && (!hasCurrent || current.ID != *input.ExpectedValueID) {
		return nil, owner.conflict(current)
	}
	write := &attributeValueWrite{}
	if hasCurrent {
		if activeFrom.Before(current.ActiveFrom) {
			return nil, fmt.Errorf("%w: active_from precedes the current value",
				ErrAttributeValueInvalid)
		}
		write.Superseded, err = s.closeAttributeValueTx(
			ctx, tx, owner, current.ID, activeFrom, transactionTime)
		if err != nil {
			return nil, err
		}
	}
	write.Value, err = s.insertAttributeValueTx(
		ctx, tx, owner, definition, input, ordinal, activeFrom, transactionTime)
	if err != nil {
		return nil, err
	}
	return write, nil
}

// supersedeAttributeValueTx closes the current value in a slot without a
// replacement. Retired definitions stay retractable.
func (s *Store) supersedeAttributeValueTx(
	ctx context.Context, tx *loggedTx, owner attributeOwner, definition AttributeDefinition,
	ownerID int64, ordinalInput, expectedValueID *int64, at, transactionTime time.Time,
) (*attributeValueRow, error) {
	if err := retractableAttributeDefinition(definition); err != nil {
		return nil, err
	}
	if definition.ValueType == AttributeValueRecordReference {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return nil, err
		}
	}
	ordinal := int64(0)
	if ordinalInput != nil {
		if definition.Cardinality == AttributeCardinalitySingle && *ordinalInput != 0 {
			return nil, fmt.Errorf(
				"%w: ordinal %d is not allowed on %s, which declares cardinality single",
				ErrAttributeValueInvalid, *ordinalInput, definition.Slug)
		}
		ordinal = *ordinalInput
	}
	if err := owner.lockOwner(ctx, s, tx, ownerID, false); err != nil {
		return nil, err
	}
	current, hasCurrent, err := s.currentAttributeValueTx(
		ctx, tx, owner, ownerID, definition.ID, ordinal)
	if err != nil {
		return nil, err
	}
	if !hasCurrent {
		return nil, ErrAttributeValueNotFound
	}
	if expectedValueID != nil && current.ID != *expectedValueID {
		return nil, owner.conflict(current)
	}
	if at.Before(current.ActiveFrom) {
		return nil, fmt.Errorf("%w: supersede time precedes active_from",
			ErrAttributeValueInvalid)
	}
	return s.closeAttributeValueTx(ctx, tx, owner, current.ID, at, transactionTime)
}

func (s *Store) verifyAttributeRecordTargetTx(
	ctx context.Context, tx *loggedTx, value AttributeValue,
) error {
	if value.Type != AttributeValueRecordReference {
		return nil
	}
	switch *value.RecordType {
	case string(AttributeObjectPerson):
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

func (s *Store) currentAttributeValueTx(
	ctx context.Context, tx *loggedTx, owner attributeOwner,
	ownerID, definitionID, ordinal int64,
) (*attributeValueRow, bool, error) {
	value, err := scanAttributeValueRow(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %s v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.%s = ? AND v.definition_id = ? AND v.ordinal = ?
		  AND v.active_until IS NULL AND v.superseded_at IS NULL%s
	`, owner.columns(), owner.table, owner.column, s.dialect.SelectForUpdate()),
		ownerID, definitionID, ordinal))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load current attribute value: %w", err)
	}
	return value, true, nil
}

func (s *Store) closeAttributeValueTx(
	ctx context.Context, tx *loggedTx, owner attributeOwner, valueID int64,
	activeUntil, supersededAt time.Time,
) (*attributeValueRow, error) {
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s
		SET active_until = ?, superseded_at = ?
		WHERE id = ? AND active_until IS NULL AND superseded_at IS NULL
	`, owner.table), activeUntil, supersededAt, valueID)
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
	closed, err := s.attributeValueByIDTx(ctx, tx, owner, valueID)
	if err != nil {
		return nil, fmt.Errorf("re-read closed attribute value %d: %w", valueID, err)
	}
	return closed, nil
}

func (s *Store) insertAttributeValueTx(
	ctx context.Context, tx *loggedTx, owner attributeOwner, definition AttributeDefinition,
	input attributeValueInput, ordinal int64, activeFrom, transactionTime time.Time,
) (*attributeValueRow, error) {
	var jsonValue any
	if len(input.Value.JSON) > 0 {
		jsonValue = string(input.Value.JSON)
	}
	var activeUntil any
	if input.ActiveUntil != nil {
		activeUntil = input.ActiveUntil.UTC()
	}
	args := []any{
		input.OwnerID, definition.ID, ordinal,
		input.Value.Text, input.Value.Integer, input.Value.Real, input.Value.Boolean,
		input.Value.Date, input.Value.Timestamp, jsonValue,
		input.Value.RecordType, input.Value.RecordID,
		activeFrom, activeUntil, string(input.Source), input.SourceRef,
		input.Confidence, input.Actor,
	}
	createdAtColumn, createdAtBind := "", ""
	if owner.stampCreatedAt {
		createdAtColumn, createdAtBind = ", created_at", ", ?"
		args = append(args, transactionTime)
	}
	var insertedID int64
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (
		    %s, definition_id, ordinal,
		    value_text, value_integer, value_real, value_boolean,
		    value_date, value_timestamp, value_json,
		    value_record_type, value_record_id,
		    active_from, active_until, source, source_ref, confidence, actor%s
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, %s, ?, ?, ?, ?, ?, ?, ?, ?%s)
		RETURNING id
	`, owner.table, owner.column, createdAtColumn, s.dialect.JSONBindExpr(), createdAtBind),
		args...,
	).Scan(&insertedID); err != nil {
		return nil, fmt.Errorf("insert attribute value for %s: %w", definition.Slug, err)
	}
	inserted, err := s.attributeValueByIDTx(ctx, tx, owner, insertedID)
	if err != nil {
		return nil, fmt.Errorf("re-read inserted attribute value: %w", err)
	}
	return inserted, nil
}

func (s *Store) attributeValueByIDTx(
	ctx context.Context, tx *loggedTx, owner attributeOwner, valueID int64,
) (*attributeValueRow, error) {
	return scanAttributeValueRow(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %s v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.id = ?
	`, owner.columns(), owner.table), valueID))
}

func scanAttributeValueRow(row scanner) (*attributeValueRow, error) {
	var (
		value        attributeValueRow
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
		&value.ID, &value.OwnerID, &value.DefinitionID, &value.DefinitionSlug,
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
		value.Value.JSON = jsontext.Value(append([]byte(nil), rawJSON...))
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
