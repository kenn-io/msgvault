package store

import (
	"context"
	"errors"
	"fmt"
)

const (
	AttributeUniversalIDPrimaryChannel   = "59e9a7d3-4904-4d0e-97d1-d0680e1e9e55"
	AttributeUniversalIDContactFrequency = "34b52841-dcf5-40c6-a24e-628b049e85b2"
	AttributeUniversalIDAskMeAbout       = "93c658a1-2346-4a6e-98c2-abfa29209334"
	AttributeUniversalIDLastContacted    = "6e843902-1685-4e23-a107-819220c7dd8d"
)

const (
	AttributeSlugPrimaryChannel   = "primary_channel"
	AttributeSlugContactFrequency = "contact_frequency"
	AttributeSlugAskMeAbout       = "ask_me_about"
	AttributeSlugLastContacted    = "last_contacted"
)

// AttributeDerivedSourceActivitySpine names the future last-contacted producer.
const AttributeDerivedSourceActivitySpine = "activity_spine"

// SeededAttributeDefinitions returns the deliberately small shipped set.
func SeededAttributeDefinitions() []AttributeDefinitionInput {
	stringPtr := func(value string) *string { return &value }
	return []AttributeDefinitionInput{
		{
			UniversalID:  AttributeUniversalIDPrimaryChannel,
			ObjectType:   AttributeObjectPerson,
			Slug:         AttributeSlugPrimaryChannel,
			Label:        "Primary channel",
			Description:  stringPtr("Preferred way to reach this person"),
			ValueType:    AttributeValueText,
			FieldType:    AttributeFieldSelect,
			Cardinality:  AttributeCardinalitySingle,
			DisplayOrder: 10,
			Ownership:    AttributeOwnershipSystem,
			UICreatable:  true,
			UIEditable:   true,
			APIMutable:   true,
			IsAudited:    true,
			IsDeletable:  false,
			Options: &AttributeOptions{Choices: []AttributeChoice{
				{Value: MessageTypeEmail, Label: "Email"},
				{Value: "phone", Label: "Phone"},
				{Value: "sms", Label: "SMS"},
				{Value: "chat", Label: "Chat"},
				{Value: "in_person", Label: "In person"},
			}},
		},
		{
			UniversalID:  AttributeUniversalIDContactFrequency,
			ObjectType:   AttributeObjectPerson,
			Slug:         AttributeSlugContactFrequency,
			Label:        "Contact frequency",
			Description:  stringPtr("Desired number of days between contacts"),
			ValueType:    AttributeValueInteger,
			FieldType:    AttributeFieldDuration,
			Cardinality:  AttributeCardinalitySingle,
			DisplayOrder: 20,
			Ownership:    AttributeOwnershipSystem,
			UICreatable:  true,
			UIEditable:   true,
			APIMutable:   true,
			IsAudited:    true,
			IsDeletable:  false,
			Options:      &AttributeOptions{Unit: "days"},
		},
		{
			UniversalID:  AttributeUniversalIDAskMeAbout,
			ObjectType:   AttributeObjectPerson,
			Slug:         AttributeSlugAskMeAbout,
			Label:        "Ask me about",
			Description:  stringPtr("Topics worth raising with this person"),
			ValueType:    AttributeValueText,
			FieldType:    AttributeFieldText,
			Cardinality:  AttributeCardinalityMulti,
			DisplayOrder: 30,
			Ownership:    AttributeOwnershipSystem,
			UICreatable:  true,
			UIEditable:   true,
			APIMutable:   true,
			IsSearchable: true,
			IsAudited:    true,
			IsDeletable:  false,
			Options:      &AttributeOptions{MaxLength: 120},
		},
		{
			UniversalID:   AttributeUniversalIDLastContacted,
			ObjectType:    AttributeObjectPerson,
			Slug:          AttributeSlugLastContacted,
			Label:         "Last contacted",
			Description:   stringPtr("Most recent interaction, computed from archive activity"),
			ValueType:     AttributeValueTimestamp,
			FieldType:     AttributeFieldTimestamp,
			Cardinality:   AttributeCardinalitySingle,
			DisplayOrder:  40,
			Ownership:     AttributeOwnershipSystem,
			IsAudited:     false,
			IsDeletable:   false,
			HistoryExempt: true,
			DerivedSource: stringPtr(AttributeDerivedSourceActivitySpine),
		},
	}
}

// EnsureSeededAttributeDefinitions reconciles shipped definitions.
func (s *Store) EnsureSeededAttributeDefinitions() error {
	return s.EnsureSeededAttributeDefinitionsContext(context.Background())
}

// EnsureSeededAttributeDefinitionsContext creates missing seeds and repairs structure.
func (s *Store) EnsureSeededAttributeDefinitionsContext(ctx context.Context) error {
	for _, seed := range SeededAttributeDefinitions() {
		existing, err := s.GetAttributeDefinitionBySlugContext(ctx, seed.ObjectType, seed.Slug)
		switch {
		case errors.Is(err, ErrAttributeDefinitionNotFound):
			if _, err := s.CreateAttributeDefinitionContext(ctx, seed); err != nil &&
				!errors.Is(err, ErrAttributeDefinitionUniversalIDConflict) &&
				!errors.Is(err, ErrAttributeDefinitionSlugConflict) {
				return fmt.Errorf("seed attribute definition %s: %w", seed.Slug, err)
			}
			continue
		case err != nil:
			return fmt.Errorf("load seeded attribute definition %s: %w", seed.Slug, err)
		}
		if err := s.reconcileSeededDefinition(ctx, existing, seed); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reconcileSeededDefinition(
	ctx context.Context, existing *AttributeDefinition, seed AttributeDefinitionInput,
) error {
	validated, err := validateAttributeDefinitionInput(seed)
	if err != nil {
		return fmt.Errorf("validate seeded attribute definition %s: %w", seed.Slug, err)
	}
	if !seededDefinitionDiffers(existing, validated) {
		return nil
	}
	options, err := marshalAttributeOptions(validated.Options)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`
		UPDATE attribute_definitions
		SET value_type = ?, field_type = ?, record_target = ?, cardinality = ?,
		    is_required = ?, ownership = ?, ui_creatable = ?,
		    ui_editable = ?, api_mutable = ?, is_searchable = ?, is_audited = ?,
		    is_deletable = ?, history_exempt = ?, derived_source = ?,
		    options = %s, vcard_property = ?,
		    revision = revision + 1, updated_at = %s
		WHERE universal_id = ?
	`, s.dialect.JSONBindExpr(), s.dialect.Now())
	if _, err := s.db.ExecContext(ctx, query,
		string(validated.ValueType), string(validated.FieldType), validated.RecordTarget,
		string(validated.Cardinality), validated.IsRequired,
		string(validated.Ownership), validated.UICreatable, validated.UIEditable,
		validated.APIMutable, validated.IsSearchable, validated.IsAudited,
		validated.IsDeletable, validated.HistoryExempt, validated.DerivedSource,
		options, validated.VCardProperty, validated.UniversalID,
	); err != nil {
		return fmt.Errorf("reconcile seeded attribute definition %s: %w", seed.Slug, err)
	}
	return nil
}

// seededDefinitionDiffers ignores label, description, and display_order:
// those are user-adjustable presentation fields that reseeding must preserve.
func seededDefinitionDiffers(
	existing *AttributeDefinition, seed AttributeDefinitionInput,
) bool {
	if existing.ValueType != seed.ValueType ||
		existing.FieldType != seed.FieldType ||
		existing.Cardinality != seed.Cardinality ||
		existing.Ownership != seed.Ownership ||
		existing.IsRequired != seed.IsRequired ||
		existing.UICreatable != seed.UICreatable ||
		existing.UIEditable != seed.UIEditable ||
		existing.APIMutable != seed.APIMutable ||
		existing.IsSearchable != seed.IsSearchable ||
		existing.IsAudited != seed.IsAudited ||
		existing.IsDeletable != seed.IsDeletable ||
		existing.HistoryExempt != seed.HistoryExempt {
		return true
	}
	if !equalOptionalString(existing.RecordTarget, seed.RecordTarget) ||
		!equalOptionalString(existing.DerivedSource, seed.DerivedSource) ||
		!equalOptionalString(existing.VCardProperty, seed.VCardProperty) {
		return true
	}
	return !equalAttributeOptions(existing.Options, seed.Options)
}

func equalOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalAttributeOptions(a, b *AttributeOptions) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Unit != b.Unit || a.MaxLength != b.MaxLength || len(a.Choices) != len(b.Choices) {
		return false
	}
	for i := range a.Choices {
		if a.Choices[i] != b.Choices[i] {
			return false
		}
	}
	return true
}
