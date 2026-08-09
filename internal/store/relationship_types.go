package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

var (
	ErrRelationshipTypeNotFound = errors.New("relationship type not found")
)

// relatedTypeValues is the set of TYPE parameter values that name a
// person-to-person RELATION, in registry order.
//
// Source: RFC 6350 Section 6.6.6's type-param-related ABNF production, which
// matches the IANA vCard Elements registry parameter-values table one-to-one
// (registry last updated 2026-01-13). RFC 9554 and RFC 9555 add nothing to
// RELATED/TYPE. RFC 6350's value ABNF is "RELATED-value = URI / text" and
// defines no default TYPE.
//
// Deliberately NOT included: "work" and "home". Both are legal TYPE values on
// RELATED, reaching it through the shared multi-property registry row of
// RFC 6350 Section 5.6, so the registry's accept-set for TYPE on RELATED is 22
// values while type-param-related admits only these 20. They are excluded
// because they qualify the CONTEXT of a relation rather than naming one:
// neither can supply a meaningful forward/reverse label pair, so neither can
// name a relationship type. An imported RELATED;TYPE=work therefore matches no
// type and is staged for review, which is the correct conservative outcome.
//
// This literal is deliberately self-contained: roadmap PR 6 does not depend
// on PR 1, so it does not import a vendored registry snapshot (nothing in the
// repository vendors these CSVs yet). PR 8 depends on both and owns the test
// that cross-checks this list against that snapshot once it exists.
var relatedTypeValues = []string{
	"contact", "acquaintance", "friend", "met", "co-worker", "colleague",
	"co-resident", "neighbor", "child", "parent", "sibling", "spouse", "kin",
	"muse", "crush", "date", "sweetheart", "me", "agent", "emergency",
}

// RelatedTypeValues returns a copy of the registered RELATED TYPE values.
func RelatedTypeValues() []string {
	return append([]string(nil), relatedTypeValues...)
}

// unmappedRelatedTypes records registered RELATED TYPE values that Msgvault
// deliberately does not seed as a person relationship type, each with the
// reason. Keeping the exclusion explicit means every registered value is
// accounted for, so a silent gap cannot hide behind a shorter seed list.
var unmappedRelatedTypes = map[string]string{
	"me": "asserts that the related entity is the vCard owner, which is an " +
		"identity claim rather than a relationship; Msgvault models identity " +
		"through participant bindings and person merge, so an imported " +
		"RELATED;TYPE=me is staged for review instead of becoming an edge",
}

// UnmappedRelatedTypes returns a copy of the deliberately unseeded RELATED
// TYPE values mapped to the reason each is excluded.
func UnmappedRelatedTypes() map[string]string {
	excluded := make(map[string]string, len(unmappedRelatedTypes))
	maps.Copy(excluded, unmappedRelatedTypes)
	return excluded
}

// RelationshipTypeOwnership separates who owns a relationship type from the
// four mutability concerns. It is TEXT rather than a boolean so a later vendor
// or plugin ownership kind does not require a SQLite table rebuild.
type RelationshipTypeOwnership string

const (
	RelationshipTypeOwnershipSystem RelationshipTypeOwnership = "system"
	RelationshipTypeOwnershipUser   RelationshipTypeOwnership = "user"
)

func (o RelationshipTypeOwnership) Valid() bool {
	return o == RelationshipTypeOwnershipSystem || o == RelationshipTypeOwnershipUser
}

// RelationshipType is presentation and interchange metadata for one kind of
// person relationship. See the relationship_types table comment for the
// forward/reverse label contract and the orientation rules.
type RelationshipType struct {
	ID               int64                     `json:"id"`
	UniversalID      string                    `json:"universal_id"`
	Slug             string                    `json:"slug"`
	ForwardLabel     string                    `json:"forward_label"`
	ReverseLabel     string                    `json:"reverse_label"`
	IsSymmetric      bool                      `json:"is_symmetric"`
	IsCanonical      bool                      `json:"is_canonical"`
	InverseTypeID    *int64                    `json:"inverse_type_id,omitempty"`
	VCardRelatedType *string                   `json:"vcard_related_type,omitempty"`
	Color            *string                   `json:"color,omitempty"`
	Icon             *string                   `json:"icon,omitempty"`
	Description      *string                   `json:"description,omitempty"`
	Ownership        RelationshipTypeOwnership `json:"ownership"`
	IsDeletable      bool                      `json:"is_deletable"`
	Revision         int64                     `json:"revision"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

// RelationshipDirection names which end of a canonical edge is doing the
// viewing. It is a presentation concept: both values describe the same row.
type RelationshipDirection string

const (
	// RelationshipDirectionOutgoing means the viewing person is the edge's
	// source, so the counterpart is labelled with the type's reverse label.
	RelationshipDirectionOutgoing RelationshipDirection = "outgoing"
	// RelationshipDirectionIncoming means the viewing person is the edge's
	// target, so the counterpart is labelled with the type's forward label.
	RelationshipDirectionIncoming RelationshipDirection = "incoming"
)

// CounterpartLabel returns the label for the person at the other end of an
// edge of this type, given which endpoint is doing the viewing.
//
// Viewing from the source endpoint yields ReverseLabel, because the row
// asserts "source is the ForwardLabel of target" and therefore implies
// "target is the ReverseLabel of source".
func (t RelationshipType) CounterpartLabel(direction RelationshipDirection) string {
	if direction == RelationshipDirectionOutgoing {
		return t.ReverseLabel
	}
	return t.ForwardLabel
}

const relationshipTypeColumns = `
	id, universal_id, slug, forward_label, reverse_label,
	is_symmetric, is_canonical, inverse_type_id, vcard_related_type,
	color, icon, description, ownership, is_deletable,
	revision, created_at, updated_at
`

func (s *Store) ListRelationshipTypesContext(ctx context.Context) ([]RelationshipType, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+relationshipTypeColumns+` FROM relationship_types ORDER BY LOWER(slug), id`)
	if err != nil {
		return nil, fmt.Errorf("list relationship types: %w", err)
	}
	defer func() { _ = rows.Close() }()

	types := make([]RelationshipType, 0, len(systemRelationshipTypes))
	for rows.Next() {
		relationshipType, scanErr := scanRelationshipType(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan relationship type: %w", scanErr)
		}
		types = append(types, *relationshipType)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relationship types: %w", err)
	}
	return types, nil
}

func (s *Store) GetRelationshipTypeContext(ctx context.Context, id int64) (*RelationshipType, error) {
	relationshipType, err := scanRelationshipType(s.db.QueryRowContext(ctx,
		`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRelationshipTypeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get relationship type %d: %w", id, err)
	}
	return relationshipType, nil
}

// GetRelationshipTypeBySlugContext looks a type up by its immutable machine
// slug, which is the stable wire key used by the API and CLI.
func (s *Store) GetRelationshipTypeBySlugContext(ctx context.Context, slug string) (*RelationshipType, error) {
	relationshipType, err := scanRelationshipType(s.db.QueryRowContext(ctx,
		`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrRelationshipTypeNotFound, slug)
	}
	if err != nil {
		return nil, fmt.Errorf("get relationship type %q: %w", slug, err)
	}
	return relationshipType, nil
}

func scanRelationshipType(row scanner) (*RelationshipType, error) {
	var (
		relationshipType RelationshipType
		inverseTypeID    sql.NullInt64
		relatedType      sql.NullString
		color            sql.NullString
		icon             sql.NullString
		description      sql.NullString
		ownership        string
	)
	if err := row.Scan(
		&relationshipType.ID, &relationshipType.UniversalID, &relationshipType.Slug,
		&relationshipType.ForwardLabel, &relationshipType.ReverseLabel,
		&relationshipType.IsSymmetric, &relationshipType.IsCanonical,
		&inverseTypeID, &relatedType, &color, &icon, &description,
		&ownership, &relationshipType.IsDeletable,
		&relationshipType.Revision, &relationshipType.CreatedAt, &relationshipType.UpdatedAt,
	); err != nil {
		return nil, err
	}
	relationshipType.Ownership = RelationshipTypeOwnership(ownership)
	if inverseTypeID.Valid {
		relationshipType.InverseTypeID = &inverseTypeID.Int64
	}
	if relatedType.Valid {
		relationshipType.VCardRelatedType = &relatedType.String
	}
	if color.Valid {
		relationshipType.Color = &color.String
	}
	if icon.Valid {
		relationshipType.Icon = &icon.String
	}
	if description.Valid {
		relationshipType.Description = &description.String
	}
	return &relationshipType, nil
}

var (
	ErrRelationshipTypeInvalid             = errors.New("invalid relationship type")
	ErrRelationshipTypeSlugConflict        = errors.New("relationship type slug already exists")
	ErrRelationshipTypeRelatedTypeConflict = errors.New("vCard RELATED type is already mapped")
	ErrRelationshipTypeRevisionConflict    = errors.New("relationship type revision conflict")
	ErrRelationshipTypeNotDeletable        = errors.New("relationship type is not deletable")
	ErrRelationshipTypeInUse               = errors.New("relationship type is referenced by person relationships")
	ErrRelationshipTypeSymmetricLabels     = errors.New("a symmetric relationship type requires identical forward and reverse labels")
)

// maxRelationshipTypeSlugBytes bounds the immutable machine slug. It is well
// under PostgreSQL's btree key limit and long enough for any real label.
const maxRelationshipTypeSlugBytes = 64

// maxRelationshipTypeLabelBytes bounds mutable presentation strings so a
// hostile import cannot store an unbounded label.
const maxRelationshipTypeLabelBytes = 128

// RelationshipTypeInput creates a user-owned relationship type. User types
// are always their own canonical orientation: defining a second type as the
// inverse of an existing one is deliberately not supported, because the only
// registry-mandated inverse pair (parent/child) is seeded.
type RelationshipTypeInput struct {
	Slug             string
	ForwardLabel     string
	ReverseLabel     string
	IsSymmetric      bool
	VCardRelatedType *string
	Color            *string
	Icon             *string
	Description      *string
}

// RelationshipTypeUpdate edits mutable presentation and interchange metadata.
// A nil field is left unchanged. A pointer to the empty string clears a
// nullable field (colour, icon, description, vCard RELATED mapping); the two
// labels are NOT NULL and reject an empty value.
//
// Slug, universal ID, symmetry, canonical orientation, inverse type, and
// system/deletable ownership are immutable: stored edges depend on them, so
// changing one would silently reinterpret existing data.
type RelationshipTypeUpdate struct {
	ForwardLabel     *string
	ReverseLabel     *string
	VCardRelatedType *string
	Color            *string
	Icon             *string
	Description      *string
}

func (s *Store) CreateRelationshipTypeContext(
	ctx context.Context, input RelationshipTypeInput,
) (*RelationshipType, error) {
	slug := strings.TrimSpace(input.Slug)
	if err := validateRelationshipTypeSlug(slug); err != nil {
		return nil, err
	}
	forward, err := validateRelationshipLabel("forward_label", input.ForwardLabel)
	if err != nil {
		return nil, err
	}
	reverse, err := validateRelationshipLabel("reverse_label", input.ReverseLabel)
	if err != nil {
		return nil, err
	}
	if input.IsSymmetric && forward != reverse {
		return nil, fmt.Errorf("%w: %q != %q", ErrRelationshipTypeSymmetricLabels, forward, reverse)
	}
	relatedType, err := validateVCardRelatedType(input.VCardRelatedType)
	if err != nil {
		return nil, err
	}
	// A user-created type gets an opaque random UUID, minted server-side and
	// never accepted from a client. This reuses the existing unexported
	// generator rather than adding a second one, so every externally visible
	// Msgvault identity shares one format. (Roadmap PR 2 wraps the same
	// function as store.NewAttributeUniversalID for its own definitions.)
	universalID, err := newVCardUID()
	if err != nil {
		return nil, err
	}
	created, err := scanRelationshipType(s.db.QueryRowContext(ctx, `
		INSERT INTO relationship_types (
			universal_id, slug, forward_label, reverse_label,
			is_symmetric, is_canonical, vcard_related_type,
			color, icon, description, ownership, is_deletable
		) VALUES (?, ?, ?, ?, ?, TRUE, ?, ?, ?, ?, ?, TRUE)
		RETURNING `+relationshipTypeColumns,
		universalID, slug, forward, reverse, input.IsSymmetric, relatedType,
		normalizeNullableText(input.Color), normalizeNullableText(input.Icon),
		normalizeNullableText(input.Description),
		string(RelationshipTypeOwnershipUser),
	))
	if err != nil {
		if s.dialect.IsConflictError(err) {
			return nil, s.relationshipTypeConflict(ctx, slug, relatedType)
		}
		return nil, fmt.Errorf("create relationship type %q: %w", slug, err)
	}
	return created, nil
}

// relationshipTypeConflict turns a unique violation into the specific
// sentinel the caller needs, so an API layer can pick the right message
// without parsing driver text.
func (s *Store) relationshipTypeConflict(ctx context.Context, slug string, relatedType any) error {
	if relatedType != nil {
		var taken int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM relationship_types WHERE vcard_related_type = ?`, relatedType,
		).Scan(&taken); err == nil && taken > 0 {
			return fmt.Errorf("%w: %v", ErrRelationshipTypeRelatedTypeConflict, relatedType)
		}
	}
	return fmt.Errorf("%w: %q", ErrRelationshipTypeSlugConflict, slug)
}

func (s *Store) UpdateRelationshipTypeContext(
	ctx context.Context, id, expectedRevision int64, update RelationshipTypeUpdate,
) (*RelationshipType, error) {
	var updated *RelationshipType
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := scanRelationshipType(tx.QueryRowContext(ctx,
			`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelationshipTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("load relationship type %d: %w", id, err)
		}

		forward := current.ForwardLabel
		if update.ForwardLabel != nil {
			if forward, err = validateRelationshipLabel("forward_label", *update.ForwardLabel); err != nil {
				return err
			}
		}
		reverse := current.ReverseLabel
		if update.ReverseLabel != nil {
			if reverse, err = validateRelationshipLabel("reverse_label", *update.ReverseLabel); err != nil {
				return err
			}
		}
		if current.IsSymmetric && forward != reverse {
			return fmt.Errorf("%w: %q != %q", ErrRelationshipTypeSymmetricLabels, forward, reverse)
		}

		relatedType := nullableTextFromPointer(current.VCardRelatedType)
		if update.VCardRelatedType != nil {
			if relatedType, err = validateVCardRelatedType(update.VCardRelatedType); err != nil {
				return err
			}
		}

		// Resolve every nullable presentation column in Go. A SQL
		// COALESCE(?, column) idiom cannot express "set this to NULL", so
		// "leave unchanged" and "clear" are decided here instead.
		color := nullableTextFromPointer(current.Color)
		if update.Color != nil {
			color = normalizeNullableText(update.Color)
		}
		icon := nullableTextFromPointer(current.Icon)
		if update.Icon != nil {
			icon = normalizeNullableText(update.Icon)
		}
		description := nullableTextFromPointer(current.Description)
		if update.Description != nil {
			description = normalizeNullableText(update.Description)
		}

		var updatedID int64
		err = tx.QueryRowContext(ctx, fmt.Sprintf(`
			UPDATE relationship_types
			SET forward_label = ?, reverse_label = ?, vcard_related_type = ?,
			    color = ?, icon = ?, description = ?,
			    revision = revision + 1, updated_at = %s
			WHERE id = ? AND revision = ?
			RETURNING id
		`, s.dialect.Now()),
			forward, reverse, relatedType, color, icon, description,
			id, expectedRevision).Scan(&updatedID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelationshipTypeRevisionConflict
		}
		if err != nil {
			if s.dialect.IsConflictError(err) {
				return fmt.Errorf("%w: %v", ErrRelationshipTypeRelatedTypeConflict, relatedType)
			}
			return fmt.Errorf("update relationship type %d: %w", id, err)
		}
		updated, err = scanRelationshipType(tx.QueryRowContext(ctx,
			`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE id = ?`, updatedID))
		if err != nil {
			return fmt.Errorf("reload relationship type %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteRelationshipTypeContext removes a user-owned, unreferenced type under
// revision compare-and-swap. Seeded system types are never deletable, and a
// type still referenced by any edge (active or historical) is refused so
// relationship history cannot be silently destroyed through its metadata.
func (s *Store) DeleteRelationshipTypeContext(ctx context.Context, id, expectedRevision int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var (
			revision    int64
			isDeletable bool
			ownership   string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT revision, is_deletable, ownership FROM relationship_types WHERE id = ?`, id,
		).Scan(&revision, &isDeletable, &ownership)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelationshipTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("load relationship type %d: %w", id, err)
		}
		// Both guards, not just is_deletable: a system row must stay
		// undeletable even if a future migration or manual edit flipped its
		// flag, matching how PR 2 guards seeded attribute definitions.
		if !isDeletable || RelationshipTypeOwnership(ownership) == RelationshipTypeOwnershipSystem {
			return fmt.Errorf("%w: id %d", ErrRelationshipTypeNotDeletable, id)
		}
		if revision != expectedRevision {
			return ErrRelationshipTypeRevisionConflict
		}
		var inUse int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM person_relationships WHERE relationship_type_id = ?
		`, id).Scan(&inUse); err != nil {
			return fmt.Errorf("check relationship type %d usage: %w", id, err)
		}
		if inUse > 0 {
			return fmt.Errorf("%w: id %d is used by %d relationships", ErrRelationshipTypeInUse, id, inUse)
		}
		result, err := tx.ExecContext(ctx,
			`DELETE FROM relationship_types WHERE id = ? AND revision = ?`, id, expectedRevision,
		)
		if err != nil {
			return fmt.Errorf("delete relationship type %d: %w", id, err)
		}
		return checkRelationshipTypeDeleteCASResult(result, id)
	})
}

// checkRelationshipTypeDeleteCASResult converts a zero-row DELETE into the
// revision conflict required by the delete compare-and-swap contract. A
// concurrent update can change the revision after the initial read, leaving a
// successful statement that matched no rows.
func checkRelationshipTypeDeleteCASResult(result sql.Result, id int64) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted relationship type %d: %w", id, err)
	}
	if affected == 0 {
		return ErrRelationshipTypeRevisionConflict
	}
	return nil
}

// validateRelationshipTypeSlug enforces a lowercase, hyphen-separated machine
// name. The slug is immutable and appears in URLs and CLI arguments, so it
// must be stable, unambiguous, and free of case or separator variants.
func validateRelationshipTypeSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("%w: slug is required", ErrRelationshipTypeInvalid)
	}
	if len(slug) > maxRelationshipTypeSlugBytes {
		return fmt.Errorf("%w: slug exceeds %d bytes", ErrRelationshipTypeInvalid, maxRelationshipTypeSlugBytes)
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return fmt.Errorf("%w: slug %q must not start, end, or double up on '-'", ErrRelationshipTypeInvalid, slug)
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("%w: slug %q must match [a-z0-9-]", ErrRelationshipTypeInvalid, slug)
		}
	}
	return nil
}

func validateRelationshipLabel(field, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: %s is required", ErrRelationshipTypeInvalid, field)
	}
	if len(trimmed) > maxRelationshipTypeLabelBytes {
		return "", fmt.Errorf("%w: %s exceeds %d bytes", ErrRelationshipTypeInvalid, field, maxRelationshipTypeLabelBytes)
	}
	return trimmed, nil
}

// validateVCardRelatedType accepts nil (no mapping), a pointer to the empty
// string (clear the mapping), or one of the registered RELATED TYPE values.
// An unregistered value is refused so the column cannot claim an interchange
// mapping that no vCard client understands.
//
// It returns sql.NullString rather than `any`: the `nilnil` linter
// (.golangci.yml:68) forbids returning (nil, nil) from a function whose first
// result is a pointer, interface, map, channel, or func, and a concrete
// null-able struct expresses "absent" without tripping it.
func validateVCardRelatedType(value *string) (sql.NullString, error) {
	if value == nil {
		return sql.NullString{}, nil
	}
	trimmed := strings.ToLower(strings.TrimSpace(*value))
	if trimmed == "" {
		return sql.NullString{}, nil
	}
	if slices.Contains(relatedTypeValues, trimmed) {
		return sql.NullString{String: trimmed, Valid: true}, nil
	}
	return sql.NullString{}, fmt.Errorf("%w: %q is not a registered vCard RELATED TYPE value",
		ErrRelationshipTypeInvalid, *value)
}

// normalizeNullableText turns an absent or blank optional string into a SQL
// NULL so "" and NULL cannot both mean "unset".
func normalizeNullableText(value *string) any {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

// nullableTextFromPointer preserves an already-stored optional value when an
// update leaves that column alone.
func nullableTextFromPointer(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
