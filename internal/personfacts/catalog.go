package personfacts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const catalogVersion = "1"

// BuildCatalog constructs the inference target catalog from portable attribute
// definitions and fixed structured targets.
func BuildCatalog(definitions []Definition, opts CatalogOptions) (Catalog, error) {
	targets := []TargetDescriptor{employmentDescriptor()}
	for _, definition := range definitions {
		description, ok := inferenceDescription(definition.Description)
		if !ok || !eligibleDefinition(definition, opts) {
			continue
		}
		targets = append(targets, TargetDescriptor{
			Kind:         TargetAttribute,
			Key:          definition.UniversalID,
			UniversalID:  definition.UniversalID,
			Slug:         definition.Slug,
			Description:  description,
			ValueType:    definition.ValueType,
			Cardinality:  definition.Cardinality,
			RecordTarget: definition.RecordTarget,
			MaxLength:    definition.MaxLength,
			Choices:      cloneChoices(definition.Choices),
			Sensitive:    definition.Sensitive,
		})
	}

	targets = canonicalTargets(targets)
	for i := range targets {
		revision, err := DescriptorRevision(targets[i])
		if err != nil {
			return Catalog{}, err
		}
		targets[i].Revision = revision
	}
	fingerprint, err := CatalogFingerprint(targets)
	if err != nil {
		return Catalog{}, err
	}
	return Catalog{Version: catalogVersion, Fingerprint: fingerprint, Targets: targets}, nil
}

// DescriptorRevision hashes the inference-relevant content of one target.
func DescriptorRevision(target TargetDescriptor) (string, error) {
	target = canonicalTarget(target)
	target.Revision = ""
	encoded, err := json.Marshal(target)
	if err != nil {
		return "", fmt.Errorf("encode descriptor revision: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

// CatalogFingerprint hashes the sorted target descriptors, including their
// descriptor revisions.
func CatalogFingerprint(targets []TargetDescriptor) (string, error) {
	encoded, err := json.Marshal(canonicalTargets(targets))
	if err != nil {
		return "", fmt.Errorf("encode catalog fingerprint: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

func eligibleDefinition(definition Definition, opts CatalogOptions) bool {
	if !definition.Active || !definition.APIMutable || definition.Derived ||
		(definition.Sensitive && !opts.IncludeSensitive) || definition.RecordTarget != "" {
		return false
	}
	switch definition.ValueType {
	case ValueText, ValueInteger, ValueReal, ValueBoolean, ValueDate, ValueTimestamp:
		return true
	default:
		return false
	}
}

func inferenceDescription(value string) (string, bool) {
	normalized := strings.TrimSpace(value)
	return normalized, normalized != "" && utf8.RuneCountInString(normalized) <= 280
}

func employmentDescriptor() TargetDescriptor {
	return TargetDescriptor{
		Kind:        TargetEmployment,
		Key:         "system:employment",
		UniversalID: "system:employment",
		Slug:        "employment",
		Description: "Current and historical employment, including organization, title, role, department, location, and partial start and end dates",
		ValueType:   ValueEmployment,
		Cardinality: CardinalityMulti,
		Fields: []FieldDescriptor{
			{Name: "organization", ValueType: ValueOrganization, Cardinality: CardinalitySingle, Required: true},
			{Name: "title", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "role", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "department", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "location", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "start_date", ValueType: ValuePartialDate, Cardinality: CardinalitySingle},
			{Name: "end_date", ValueType: ValuePartialDate, Cardinality: CardinalitySingle},
		},
	}
}

func canonicalTargets(targets []TargetDescriptor) []TargetDescriptor {
	cloned := make([]TargetDescriptor, len(targets))
	for i := range targets {
		cloned[i] = canonicalTarget(targets[i])
	}
	sort.Slice(cloned, func(i, j int) bool {
		if cloned[i].Kind != cloned[j].Kind {
			return cloned[i].Kind < cloned[j].Kind
		}
		return cloned[i].Key < cloned[j].Key
	})
	return cloned
}

func canonicalTarget(target TargetDescriptor) TargetDescriptor {
	target.Choices = cloneChoices(target.Choices)
	sort.Slice(target.Choices, func(i, j int) bool {
		if target.Choices[i].Value != target.Choices[j].Value {
			return target.Choices[i].Value < target.Choices[j].Value
		}
		return target.Choices[i].Label < target.Choices[j].Label
	})
	target.Fields = append([]FieldDescriptor(nil), target.Fields...)
	return target
}

func cloneChoices(choices []ChoiceDescriptor) []ChoiceDescriptor {
	return append([]ChoiceDescriptor(nil), choices...)
}
