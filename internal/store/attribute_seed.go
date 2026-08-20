package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var errSeededAttributeDefinitionCreateConflict = errors.New(
	"seeded attribute definition was created concurrently")

const maxSeededAttributeDefinitionAttempts = 5

const (
	AttributeUniversalIDPrimaryChannel        = "59e9a7d3-4904-4d0e-97d1-d0680e1e9e55"
	AttributeUniversalIDContactFrequency      = "34b52841-dcf5-40c6-a24e-628b049e85b2"
	AttributeUniversalIDAskMeAbout            = "93c658a1-2346-4a6e-98c2-abfa29209334"
	AttributeUniversalIDLastContacted         = "6e843902-1685-4e23-a107-819220c7dd8d"
	AttributeUniversalIDLocation              = "2068efb0-9808-498b-ac3a-8e0a87d4513a"
	AttributeUniversalIDBirthplace            = "cd5aaad0-4368-4686-85e1-4dbdb86cc54b"
	AttributeUniversalIDMembership            = "fbf748ac-585a-4f79-aac3-f7eda023b5e4"
	AttributeUniversalIDReligion              = "4425dff2-5da8-4398-8d91-9ec54defa1c0"
	AttributeUniversalIDPolitics              = "f897f0b4-45fa-469a-97d5-c7d98517a50e"
	AttributeUniversalIDPersonality           = "64142a35-ab0f-43cc-9828-87aaa362c693"
	AttributeUniversalIDFamilyPets            = "e0775bae-cf07-4d06-9cc2-cb09a5358d82"
	AttributeUniversalIDInterestsFunNow       = "e8c815d5-3568-429b-a93b-00220b0a1b43"
	AttributeUniversalIDInterestsFunGrowingUp = "7ffd6a23-74cc-4e05-ad6b-7d765d883a94"
	AttributeUniversalIDFavoritesFood         = "12612daa-90b0-461b-b928-79d4a2bc07ba"
	AttributeUniversalIDFavoritesPlace        = "be2bccc1-c395-4bb4-8f68-02e745ea7b22"
)

const (
	AttributeSlugPrimaryChannel        = "primary_channel"
	AttributeSlugContactFrequency      = "contact_frequency"
	AttributeSlugAskMeAbout            = "ask_me_about"
	AttributeSlugLastContacted         = "last_contacted"
	AttributeSlugLocation              = "location"
	AttributeSlugBirthplace            = "birthplace"
	AttributeSlugMembership            = "membership"
	AttributeSlugReligion              = "religion"
	AttributeSlugPolitics              = "politics"
	AttributeSlugPersonality           = "personality"
	AttributeSlugFamilyPets            = "family_pets"
	AttributeSlugInterestsFunNow       = "interests_fun_now"
	AttributeSlugInterestsFunGrowingUp = "interests_fun_growing_up"
	AttributeSlugFavoritesFood         = "favorites_food"
	AttributeSlugFavoritesPlace        = "favorites_place"
)

// AttributeDerivedSourceActivitySpine names the future last-contacted producer.
const AttributeDerivedSourceActivitySpine = "activity_spine"

// SeededAttributeDefinitions returns the deliberately small shipped set.
func SeededAttributeDefinitions() []AttributeDefinitionInput {
	stringPtr := func(value string) *string { return &value }
	definitions := []AttributeDefinitionInput{
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
	profileText := func(
		universalID, slug, label, description string, order int64,
		cardinality AttributeCardinality, sensitive, searchable bool,
	) AttributeDefinitionInput {
		return AttributeDefinitionInput{
			UniversalID: universalID, ObjectType: AttributeObjectPerson,
			Slug: slug, Label: label, Description: stringPtr(description),
			ValueType: AttributeValueText, FieldType: AttributeFieldText,
			Cardinality: cardinality, DisplayOrder: order,
			Ownership: AttributeOwnershipSystem, UICreatable: true,
			UIEditable: true, APIMutable: true, IsSearchable: searchable,
			IsSensitive: sensitive, IsAudited: true, IsDeletable: false,
			Options: &AttributeOptions{MaxLength: 120},
		}
	}
	return append(definitions,
		profileText(AttributeUniversalIDLocation, AttributeSlugLocation, "Location",
			"Where this person lives or is based", 50, AttributeCardinalitySingle, false, true),
		profileText(AttributeUniversalIDBirthplace, AttributeSlugBirthplace, "Born in",
			"Where this person was born", 60, AttributeCardinalitySingle, false, true),
		profileText(AttributeUniversalIDMembership, AttributeSlugMembership, "Membership",
			"Groups and communities this person belongs to", 70, AttributeCardinalityMulti, false, true),
		profileText(AttributeUniversalIDReligion, AttributeSlugReligion, "Religion",
			"Religious identity or affiliation", 80, AttributeCardinalitySingle, true, false),
		profileText(AttributeUniversalIDPolitics, AttributeSlugPolitics, "Politics",
			"Political views or affiliation", 90, AttributeCardinalitySingle, true, false),
		profileText(AttributeUniversalIDPersonality, AttributeSlugPersonality, "Personality",
			"Personality traits and working style", 100, AttributeCardinalityMulti, true, false),
		profileText(AttributeUniversalIDFamilyPets, AttributeSlugFamilyPets, "Pets",
			"Pets in this person's family", 110, AttributeCardinalityMulti, false, true),
		profileText(AttributeUniversalIDInterestsFunNow, AttributeSlugInterestsFunNow, "Fun now",
			"Activities this person enjoys now", 120, AttributeCardinalityMulti, false, true),
		profileText(AttributeUniversalIDInterestsFunGrowingUp, AttributeSlugInterestsFunGrowingUp, "Fun growing up",
			"Activities this person enjoyed growing up", 130, AttributeCardinalityMulti, false, true),
		profileText(AttributeUniversalIDFavoritesFood, AttributeSlugFavoritesFood, "Favorite food",
			"Foods this person especially likes", 140, AttributeCardinalityMulti, false, true),
		profileText(AttributeUniversalIDFavoritesPlace, AttributeSlugFavoritesPlace, "Favorite place",
			"Places this person especially likes", 150, AttributeCardinalityMulti, false, true),
	)
}

// EnsureSeededAttributeDefinitions reconciles shipped definitions.
func (s *Store) EnsureSeededAttributeDefinitions() error {
	return s.EnsureSeededAttributeDefinitionsContext(context.Background())
}

// EnsureSeededAttributeDefinitionsContext creates missing seeds and repairs structure.
func (s *Store) EnsureSeededAttributeDefinitionsContext(ctx context.Context) error {
	for _, seed := range SeededAttributeDefinitions() {
		if err := s.ensureSeededAttributeDefinitionContext(ctx, seed); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureSeededAttributeDefinitionContext(
	ctx context.Context, seed AttributeDefinitionInput,
) error {
	var err error
	for attempt := range maxSeededAttributeDefinitionAttempts {
		err = s.reconcileOneSeededAttributeDefinitionContext(ctx, seed)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errSeededAttributeDefinitionCreateConflict) &&
			!s.dialect.IsBusyError(err) {
			return err
		}
		if attempt+1 == maxSeededAttributeDefinitionAttempts {
			break
		}
		select {
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("reconcile seeded attribute definition %s: gave up after %d attempts: %w",
		seed.Slug, maxSeededAttributeDefinitionAttempts, err)
}

func (s *Store) reconcileOneSeededAttributeDefinitionContext(
	ctx context.Context, seed AttributeDefinitionInput,
) error {
	bySlug, err := s.GetAttributeDefinitionBySlugContext(ctx, seed.ObjectType, seed.Slug)
	if errors.Is(err, ErrAttributeDefinitionNotFound) {
		bySlug = nil
	} else if err != nil {
		return fmt.Errorf("load seeded attribute definition %s: %w", seed.Slug, err)
	}
	byUniversalID, err := s.getAttributeDefinitionByUniversalIDContext(ctx, seed.UniversalID)
	if errors.Is(err, ErrAttributeDefinitionNotFound) {
		byUniversalID = nil
	} else if err != nil {
		return fmt.Errorf("load seeded attribute definition %s by universal id: %w",
			seed.Slug, err)
	}
	if byUniversalID != nil && byUniversalID.ObjectType != seed.ObjectType {
		return fmt.Errorf("seeded attribute definition %s universal id %s belongs to object type %s, want %s",
			seed.Slug, seed.UniversalID, byUniversalID.ObjectType, seed.ObjectType)
	}
	if s.attributeSeedReadHook != nil {
		s.attributeSeedReadHook(seed.Slug)
	}

	switch {
	case bySlug == nil && byUniversalID == nil:
		if _, err := s.CreateAttributeDefinitionContext(ctx, seed); err != nil {
			if errors.Is(err, ErrAttributeDefinitionSlugConflict) ||
				errors.Is(err, ErrAttributeDefinitionUniversalIDConflict) {
				return fmt.Errorf("%w: seed attribute definition %s: %w",
					errSeededAttributeDefinitionCreateConflict, seed.Slug, err)
			}
			return fmt.Errorf("seed attribute definition %s: %w", seed.Slug, err)
		}
	case byUniversalID != nil:
		// universal_id is the portable seed identity, while slug is an
		// immutable archive-local API handle. Once a seed exists, preserve its
		// assigned slug even when the preferred shipped slug is occupied.
		if err := s.reconcileSeededDefinition(ctx, byUniversalID, seed); err != nil {
			return err
		}
	case bySlug != nil:
		if err := s.installSeededDefinitionAtFallbackSlug(ctx, seed); err != nil {
			if errors.Is(err, ErrAttributeDefinitionSlugConflict) ||
				errors.Is(err, ErrAttributeDefinitionUniversalIDConflict) {
				return fmt.Errorf("%w: %w", errSeededAttributeDefinitionCreateConflict, err)
			}
			return err
		}
	}
	return nil
}

func (s *Store) getAttributeDefinitionByUniversalIDContext(
	ctx context.Context, universalID string,
) (*AttributeDefinition, error) {
	definition, err := scanAttributeDefinition(s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM attribute_definitions WHERE universal_id = ?
	`, attributeDefinitionColumns), universalID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAttributeDefinitionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get attribute definition by universal id %q: %w", universalID, err)
	}
	return definition, nil
}

func (s *Store) installSeededDefinitionAtFallbackSlug(
	ctx context.Context, seed AttributeDefinitionInput,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		fallback, err := nextSeededAttributeFallbackSlug(ctx, tx, seed)
		if err != nil {
			return err
		}
		seed.Slug = fallback
		validated, err := validateAttributeDefinitionInput(seed)
		if err != nil {
			return fmt.Errorf("validate seeded attribute definition %s: %w", seed.Slug, err)
		}
		if _, err := s.createAttributeDefinitionTx(ctx, tx, validated); err != nil {
			return fmt.Errorf("install seeded attribute definition %s at fallback slug %s: %w",
				seed.UniversalID, seed.Slug, err)
		}
		return nil
	})
}

func nextSeededAttributeFallbackSlug(
	ctx context.Context, tx *loggedTx, seed AttributeDefinitionInput,
) (string, error) {
	for attempt := 0; ; attempt++ {
		candidate := seededAttributeFallbackSlug(seed.Slug, seed.UniversalID, attempt)
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM attribute_definitions WHERE object_type = ? AND slug = ?
		`, string(seed.ObjectType), candidate).Scan(&count); err != nil {
			return "", fmt.Errorf("find fallback slug for seeded attribute definition %s: %w",
				seed.Slug, err)
		}
		if count == 0 {
			return candidate, nil
		}
	}
}

func seededAttributeFallbackSlug(slug, universalID string, attempt int) string {
	compactID := strings.ReplaceAll(universalID, "-", "")
	suffix := "_system_" + compactID
	if attempt > 0 {
		suffix += "_" + strconv.Itoa(attempt+1)
	}
	prefix := slug
	if len(prefix)+len(suffix) > maxAttributeSlugLength {
		prefix = prefix[:maxAttributeSlugLength-len(suffix)]
	}
	return prefix + suffix
}

const maxAttributeSlugLength = 63

func requireOneSeededDefinitionRow(result sql.Result, slug, action string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count %s for seeded attribute definition %s: %w", action, slug, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s for seeded attribute definition %s: updated %d rows, want 1",
			action, slug, affected)
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
	// A drifted seed can change vcard_property, which every person's native
	// projection reads, so the repair and the projection bump have to be one
	// transaction: a bump that lands separately leaves a window in which an
	// envelope commit sees the new definition and no reason to reject a
	// render made from the old one.
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.reconcileSeededDefinitionWith(ctx, tx, existing, seed); err != nil {
			return err
		}
		return s.bumpAllVCardProjectionsTx(ctx, tx)
	})
}

func (s *Store) reconcileSeededDefinitionWith(
	ctx context.Context, execer contextQuerier,
	existing *AttributeDefinition, seed AttributeDefinitionInput,
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
		    ui_editable = ?, api_mutable = ?, is_searchable = ?, is_sensitive = ?, is_audited = ?,
		    is_deletable = ?, history_exempt = ?, derived_source = ?,
		    options = %s, vcard_property = ?,
		    revision = revision + 1, updated_at = %s
		WHERE universal_id = ?
	`, s.dialect.JSONBindExpr(), s.dialect.Now())
	result, err := execer.ExecContext(ctx, query,
		string(validated.ValueType), string(validated.FieldType), validated.RecordTarget,
		string(validated.Cardinality), validated.IsRequired,
		string(validated.Ownership), validated.UICreatable, validated.UIEditable,
		validated.APIMutable, validated.IsSearchable, validated.IsSensitive, validated.IsAudited,
		validated.IsDeletable, validated.HistoryExempt, validated.DerivedSource,
		options, validated.VCardProperty, validated.UniversalID,
	)
	if err != nil {
		return fmt.Errorf("reconcile seeded attribute definition %s: %w", seed.Slug, err)
	}
	return requireOneSeededDefinitionRow(result, seed.Slug, "reconcile")
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
		existing.IsSensitive != seed.IsSensitive ||
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
