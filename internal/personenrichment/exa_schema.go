package personenrichment

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/personfacts"
)

const exaSchemaTypeKey = "type"

// BuildExaOutputSchema converts the exact requested PR 1 target descriptors
// into the closed JSON schema sent to Exa synthesized-output modes.
func BuildExaOutputSchema(targets []personfacts.TargetDescriptor) (json.RawMessage, error) {
	if len(targets) == 0 {
		return nil, errors.New("exa output schema requires at least one target")
	}
	properties := make(map[string]any, len(targets))
	required := make([]string, 0, len(targets))
	for i, target := range targets {
		if err := validateExaTargetDescriptor(target); err != nil {
			return nil, fmt.Errorf("exa output schema target %d: %w", i, err)
		}
		if _, duplicate := properties[target.Key]; duplicate {
			return nil, fmt.Errorf("exa output schema contains duplicate target %q", target.Key)
		}
		schema, err := exaTargetSchema(target)
		if err != nil {
			return nil, fmt.Errorf("exa output schema target %q: %w", target.Key, err)
		}
		properties[target.Key] = schema
		required = append(required, target.Key)
	}
	encoded, err := json.Marshal(map[string]any{
		exaSchemaTypeKey:       "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Exa output schema: %w", err)
	}
	return encoded, nil
}

func validateExaTargetDescriptor(target personfacts.TargetDescriptor) error {
	if strings.TrimSpace(target.Key) == "" || target.Key != strings.TrimSpace(target.Key) {
		return errors.New("target key must be non-empty and trimmed")
	}
	if target.UniversalID != target.Key || strings.TrimSpace(target.Slug) == "" {
		return errors.New("target identity is not an exact eligible PR 1 descriptor")
	}
	if target.Description == "" || target.Description != strings.TrimSpace(target.Description) {
		return errors.New("target description must be non-empty and trimmed")
	}
	if target.RecordTarget != "" {
		return errors.New("record-reference targets are unsupported")
	}
	wantRevision, err := personfacts.DescriptorRevision(target)
	if err != nil {
		return fmt.Errorf("derive target revision: %w", err)
	}
	if target.Revision != wantRevision {
		return errors.New("target revision does not match its exact descriptor")
	}
	switch target.Cardinality {
	case personfacts.CardinalitySingle, personfacts.CardinalityMulti:
	default:
		return fmt.Errorf("unsupported target cardinality %q", target.Cardinality)
	}
	switch target.Kind {
	case personfacts.TargetAttribute:
		if len(target.Fields) != 0 {
			return errors.New("attribute target must not declare structured fields")
		}
		if !exaSupportsScalarType(target.ValueType) {
			return fmt.Errorf("unsupported target value type %q", target.ValueType)
		}
	case personfacts.TargetEmployment:
		if err := validateExaEmploymentDescriptor(target); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported target kind %q", target.Kind)
	}
	return nil
}

func validateExaEmploymentDescriptor(target personfacts.TargetDescriptor) error {
	if target.Key != "system:employment" || target.ValueType != personfacts.ValueEmployment ||
		target.Cardinality != personfacts.CardinalityMulti {
		return errors.New("employment target is not the fixed PR 1 employment descriptor")
	}
	want := []personfacts.FieldDescriptor{
		{Name: "organization", ValueType: personfacts.ValueOrganization, Cardinality: personfacts.CardinalitySingle, Required: true},
		{Name: "title", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
		{Name: "role", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
		{Name: "department", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
		{Name: "location", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
		{Name: "start_date", ValueType: personfacts.ValuePartialDate, Cardinality: personfacts.CardinalitySingle},
		{Name: "end_date", ValueType: personfacts.ValuePartialDate, Cardinality: personfacts.CardinalitySingle},
	}
	if len(target.Fields) != len(want) {
		return errors.New("employment target fields do not match the fixed PR 1 descriptor")
	}
	for i := range want {
		if target.Fields[i] != want[i] {
			return errors.New("employment target fields do not match the fixed PR 1 descriptor")
		}
	}
	return nil
}

func exaTargetSchema(target personfacts.TargetDescriptor) (map[string]any, error) {
	valueSchema, err := exaValueSchema(target.ValueType, target.Choices, target.MaxLength)
	if err != nil {
		return nil, err
	}
	if target.Cardinality == personfacts.CardinalityMulti {
		return map[string]any{
			exaSchemaTypeKey: "array",
			"description":    target.Description,
			"items":          valueSchema,
		}, nil
	}
	valueSchema["description"] = target.Description
	return valueSchema, nil
}

func exaValueSchema(
	valueType personfacts.ValueType,
	choices []personfacts.ChoiceDescriptor,
	maxLength int,
) (map[string]any, error) {
	var schema map[string]any
	switch valueType {
	case personfacts.ValueText:
		schema = map[string]any{exaSchemaTypeKey: "string"}
	case personfacts.ValueInteger:
		schema = map[string]any{exaSchemaTypeKey: "integer"}
	case personfacts.ValueReal:
		schema = map[string]any{exaSchemaTypeKey: "number"}
	case personfacts.ValueBoolean:
		schema = map[string]any{exaSchemaTypeKey: "boolean"}
	case personfacts.ValueDate:
		schema = map[string]any{exaSchemaTypeKey: "string", "format": "date"}
	case personfacts.ValueTimestamp:
		schema = map[string]any{exaSchemaTypeKey: "string", "format": "date-time"}
	case personfacts.ValueEmployment:
		return exaEmploymentValueSchema(), nil
	case personfacts.ValueOrganization:
		return exaOrganizationSchema(), nil
	case personfacts.ValuePartialDate:
		return exaPartialDateSchema(), nil
	default:
		return nil, fmt.Errorf("unsupported target value type %q", valueType)
	}
	if maxLength > 0 {
		if valueType != personfacts.ValueText {
			return nil, errors.New("max_length is supported only for text targets")
		}
		schema["maxLength"] = maxLength
	}
	if len(choices) > 0 {
		values := make([]any, len(choices))
		seen := make(map[string]struct{}, len(choices))
		for i, choice := range choices {
			value, key, err := exaChoiceValue(valueType, choice.Value)
			if err != nil {
				return nil, fmt.Errorf("choice %d: %w", i, err)
			}
			if _, duplicate := seen[key]; duplicate {
				return nil, errors.New("choice values must be unique")
			}
			seen[key] = struct{}{}
			values[i] = value
		}
		schema["enum"] = values
	}
	return schema, nil
}

func exaChoiceValue(valueType personfacts.ValueType, raw string) (any, string, error) {
	switch valueType {
	case personfacts.ValueText, personfacts.ValueDate, personfacts.ValueTimestamp:
		return raw, "s:" + raw, nil
	case personfacts.ValueInteger:
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("parse integer choice: %w", err)
		}
		return value, "i:" + strconv.FormatInt(value, 10), nil
	case personfacts.ValueReal:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, "", fmt.Errorf("parse real choice: %w", err)
		}
		return value, "r:" + strconv.FormatFloat(value, 'g', -1, 64), nil
	case personfacts.ValueBoolean:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, "", fmt.Errorf("parse boolean choice: %w", err)
		}
		return value, "b:" + strconv.FormatBool(value), nil
	default:
		return nil, "", errors.New("choices are unsupported for structured target values")
	}
}

func exaSupportsScalarType(valueType personfacts.ValueType) bool {
	switch valueType {
	case personfacts.ValueText, personfacts.ValueInteger, personfacts.ValueReal,
		personfacts.ValueBoolean, personfacts.ValueDate, personfacts.ValueTimestamp:
		return true
	default:
		return false
	}
}

func exaEmploymentValueSchema() map[string]any {
	return map[string]any{
		exaSchemaTypeKey: "object",
		"properties": map[string]any{
			"organization": exaOrganizationSchema(),
			"title":        map[string]any{exaSchemaTypeKey: "string"},
			"role":         map[string]any{exaSchemaTypeKey: "string"},
			"department":   map[string]any{exaSchemaTypeKey: "string"},
			"location":     map[string]any{exaSchemaTypeKey: "string"},
			"start_date":   exaPartialDateSchema(),
			"end_date":     exaPartialDateSchema(),
		},
		"required":             []string{"organization"},
		"additionalProperties": false,
	}
}

func exaOrganizationSchema() map[string]any {
	return map[string]any{
		exaSchemaTypeKey: "object",
		"properties": map[string]any{
			"name":   map[string]any{exaSchemaTypeKey: "string"},
			"domain": map[string]any{exaSchemaTypeKey: "string"},
		},
		"required":             []string{"name"},
		"additionalProperties": false,
	}
}

func exaPartialDateSchema() map[string]any {
	return map[string]any{
		exaSchemaTypeKey: "object",
		"properties": map[string]any{
			"year":  map[string]any{exaSchemaTypeKey: "integer"},
			"month": map[string]any{exaSchemaTypeKey: "integer"},
			"day":   map[string]any{exaSchemaTypeKey: "integer"},
		},
		"required":             []string{"year"},
		"additionalProperties": false,
	}
}
