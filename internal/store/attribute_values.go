package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	ErrAttributeValueInvalid          = errors.New("invalid attribute value")
	ErrAttributeValueNotFound         = errors.New("attribute value not found")
	ErrAttributeValueConflict         = errors.New("attribute value changed; reload and retry")
	ErrAttributeDefinitionNotWritable = errors.New(
		"attribute definition is read-only")
	ErrAttributeDefinitionInactive = errors.New(
		"attribute definition is not active; re-activate it to set new values")
)

var errAttributeDryRun = errors.New("attribute dry run rollback")

const maxContendedWriteAttempts = 5

// retryContendedWrite retries read-then-write transactions that can fail
// snapshot upgrade (SQLITE_BUSY), deadlock, or race a unique index. Attribute
// and employment writers use the same bounded policy.
func retryContendedWrite[T any](
	s *Store, operation string, attempt func() (*T, error),
) (*T, error) {
	var lastErr error
	for range maxContendedWriteAttempts {
		write, err := attempt()
		if err == nil {
			return write, nil
		}
		if !s.dialect.IsConflictError(err) && !s.dialect.IsBusyError(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%s: gave up after %d attempts: %w",
		operation, maxContendedWriteAttempts, lastErr)
}

// AttributeValue is a typed value union with exactly one populated member.
type AttributeValue struct {
	Type       AttributeValueType `json:"type"`
	Text       *string            `json:"text,omitempty"`
	Integer    *int64             `json:"integer,omitempty"`
	Real       *float64           `json:"real,omitempty"`
	Boolean    *bool              `json:"boolean,omitempty"`
	Date       *string            `json:"date,omitempty"`
	Timestamp  *time.Time         `json:"timestamp,omitempty"`
	JSON       json.RawMessage    `json:"json,omitempty"`
	RecordType *string            `json:"record_type,omitempty"`
	RecordID   *int64             `json:"record_id,omitempty"`
}

// CanonicalString renders scalar values for choice matching.
func (v AttributeValue) CanonicalString() (string, error) {
	switch v.Type {
	case AttributeValueText:
		if v.Text == nil {
			return "", fmt.Errorf("%w: text value is required", ErrAttributeValueInvalid)
		}
		return *v.Text, nil
	case AttributeValueInteger:
		if v.Integer == nil {
			return "", fmt.Errorf("%w: integer value is required", ErrAttributeValueInvalid)
		}
		return strconv.FormatInt(*v.Integer, 10), nil
	case AttributeValueReal:
		if v.Real == nil {
			return "", fmt.Errorf("%w: real value is required", ErrAttributeValueInvalid)
		}
		return strconv.FormatFloat(*v.Real, 'g', -1, 64), nil
	case AttributeValueBoolean:
		if v.Boolean == nil {
			return "", fmt.Errorf("%w: boolean value is required", ErrAttributeValueInvalid)
		}
		return strconv.FormatBool(*v.Boolean), nil
	case AttributeValueDate:
		if v.Date == nil {
			return "", fmt.Errorf("%w: date value is required", ErrAttributeValueInvalid)
		}
		return *v.Date, nil
	case AttributeValueTimestamp:
		if v.Timestamp == nil {
			return "", fmt.Errorf("%w: timestamp value is required", ErrAttributeValueInvalid)
		}
		return v.Timestamp.UTC().Format(time.RFC3339Nano), nil
	default:
		return "", fmt.Errorf(
			"%w: value_type %s has no canonical string form", ErrAttributeValueInvalid, v.Type)
	}
}

func populatedAttributeValueFields(value AttributeValue) int {
	count := 0
	for _, populated := range []bool{
		value.Text != nil, value.Integer != nil, value.Real != nil,
		value.Boolean != nil, value.Date != nil, value.Timestamp != nil,
		len(value.JSON) > 0, value.RecordID != nil,
	} {
		if populated {
			count++
		}
	}
	return count
}

func normalizeAttributeValue(
	definition AttributeDefinition, value AttributeValue,
) (AttributeValue, error) {
	invalid := func(format string, args ...any) (AttributeValue, error) {
		return AttributeValue{}, fmt.Errorf(
			"%w: %s", ErrAttributeValueInvalid, fmt.Sprintf(format, args...))
	}
	if value.Type == "" {
		value.Type = definition.ValueType
	}
	if value.Type != definition.ValueType {
		return invalid("value_type %s does not match definition %s which declares %s",
			value.Type, definition.Slug, definition.ValueType)
	}
	if value.Type != AttributeValueRecordReference &&
		(value.RecordType != nil || value.RecordID != nil) {
		return invalid("record_type and record_id are only valid for a record reference")
	}
	if populated := populatedAttributeValueFields(value); populated > 1 {
		return invalid("exactly one typed value must be supplied, got %d", populated)
	}

	switch value.Type {
	case AttributeValueText:
		if value.Text == nil {
			return invalid("text value is required")
		}
		text := strings.TrimSpace(*value.Text)
		if text == "" {
			return invalid("text value must not be blank")
		}
		if definition.Options != nil && definition.Options.MaxLength > 0 &&
			len([]rune(text)) > definition.Options.MaxLength {
			return invalid("text value exceeds options.max_length %d",
				definition.Options.MaxLength)
		}
		value.Text = &text
	case AttributeValueInteger:
		if value.Integer == nil {
			return invalid("integer value is required")
		}
	case AttributeValueReal:
		if value.Real == nil {
			return invalid("real value is required")
		}
	case AttributeValueBoolean:
		if value.Boolean == nil {
			return invalid("boolean value is required")
		}
	case AttributeValueDate:
		if value.Date == nil {
			return invalid("date value is required")
		}
		date := strings.TrimSpace(*value.Date)
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil || parsed.Format("2006-01-02") != date {
			return invalid("date value %q must be a YYYY-MM-DD calendar date", date)
		}
		value.Date = &date
	case AttributeValueTimestamp:
		if value.Timestamp == nil {
			return invalid("timestamp value is required")
		}
		utc := value.Timestamp.UTC()
		value.Timestamp = &utc
	case AttributeValueJSON:
		if len(value.JSON) == 0 {
			return invalid("json value is required")
		}
		trimmed := json.RawMessage(strings.TrimSpace(string(value.JSON)))
		if !json.Valid(trimmed) || string(trimmed) == "null" {
			return invalid("json value must be valid and non-null")
		}
		value.JSON = trimmed
	case AttributeValueRecordReference:
		if value.RecordID == nil || *value.RecordID <= 0 {
			return invalid("record_id must be a positive integer")
		}
		if value.RecordType == nil {
			return invalid("record_type is required for a record reference")
		}
		recordType := strings.TrimSpace(*value.RecordType)
		if definition.RecordTarget == nil || recordType != *definition.RecordTarget {
			return invalid("record_type %q does not match the definition record_target",
				recordType)
		}
		value.RecordType = &recordType
	default:
		return invalid("value_type %s is not supported", value.Type)
	}

	if definition.FieldType == AttributeFieldSelect ||
		definition.FieldType == AttributeFieldMultiselect {
		canonical, err := value.CanonicalString()
		if err != nil {
			return AttributeValue{}, err
		}
		if !slices.Contains(definition.Options.ChoiceValues(), canonical) {
			return invalid("value %q is not one of the definition's options.choices",
				canonical)
		}
	}
	return value, nil
}

func validateProvenance(source Provenance, confidence *float64) error {
	if !source.Valid() {
		return fmt.Errorf("%w: source %q is not one of %s",
			ErrAttributeValueInvalid, source, ProvenanceCheckValues())
	}
	if confidence == nil {
		return nil
	}
	if source.IsDeclared() {
		return fmt.Errorf("%w: %w (source %s)",
			ErrAttributeValueInvalid, ErrConfidenceScope, source)
	}
	if *confidence < 0 || *confidence > 1 {
		return fmt.Errorf("%w: %w: %v is outside [0, 1]",
			ErrAttributeValueInvalid, ErrConfidenceScope, *confidence)
	}
	return nil
}

func writableAttributeDefinition(definition AttributeDefinition) error {
	if !definition.IsActive {
		return fmt.Errorf("%s: %w", definition.Slug, ErrAttributeDefinitionInactive)
	}
	return retractableAttributeDefinition(definition)
}

// retractableAttributeDefinition gates supersession, which stays legal on
// inactive definitions so stale values can be retired with them.
func retractableAttributeDefinition(definition AttributeDefinition) error {
	if definition.DerivedSource != nil {
		return fmt.Errorf("%s is computed by %s: %w",
			definition.Slug, *definition.DerivedSource, ErrAttributeDefinitionNotWritable)
	}
	if !definition.APIMutable {
		return fmt.Errorf("%s: %w", definition.Slug, ErrAttributeDefinitionNotWritable)
	}
	return nil
}
