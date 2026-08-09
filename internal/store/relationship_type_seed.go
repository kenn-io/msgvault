package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// systemRelationshipTypeSeed is one seeded system relationship type.
//
// UniversalID is a hardcoded opaque UUID, generated once when this table was
// authored and never recomputed, so a seeded type has the same identity in
// every install. Do not derive it from the slug and do not regenerate an
// existing one: an existing database matches seeds on this column, so changing
// a value would orphan the old row and insert a duplicate alongside it.
type systemRelationshipTypeSeed struct {
	Slug         string
	UniversalID  string
	ForwardLabel string
	ReverseLabel string
	IsSymmetric  bool
	IsCanonical  bool
	InverseSlug  string
	RelatedType  string
	Description  string
}

// systemRelationshipTypes seeds one type per registered RELATED TYPE value
// that describes a person-to-person relationship. Labels follow the
// forward/reverse contract documented on the relationship_types table:
// forward_label completes "<source> is the ___ of <target>".
//
// No gendered parent variants (father/mother/son/daughter) are seeded. The
// registry has none, and Gramps' gendered parent columns are explicitly
// rejected by the roadmap.
var systemRelationshipTypes = []systemRelationshipTypeSeed{
	{Slug: "contact", UniversalID: "dd74c978-cc03-4fce-875f-c6d03b84feeb",
		ForwardLabel: "contact", ReverseLabel: "contact", IsSymmetric: true, IsCanonical: true, RelatedType: "contact"},
	{Slug: "acquaintance", UniversalID: "dcee8148-7f92-4cdf-ac12-e33fb36a5ceb",
		ForwardLabel: "acquaintance", ReverseLabel: "acquaintance", IsSymmetric: true, IsCanonical: true, RelatedType: "acquaintance"},
	{Slug: "friend", UniversalID: "a1121fb7-8c6b-40d4-9fe1-1cb62dabbd88",
		ForwardLabel: "friend", ReverseLabel: "friend", IsSymmetric: true, IsCanonical: true, RelatedType: "friend"},
	{Slug: "met", UniversalID: "70cda530-be30-4cd5-8ef3-3ac197277d9d",
		ForwardLabel: "met", ReverseLabel: "met", IsSymmetric: true, IsCanonical: true, RelatedType: "met",
		Description: "The two people have met in person."},
	{Slug: "co-worker", UniversalID: "523f6e4a-b439-4b44-9c0a-ba5d6145c138",
		ForwardLabel: "co-worker", ReverseLabel: "co-worker", IsSymmetric: true, IsCanonical: true, RelatedType: "co-worker"},
	{Slug: "colleague", UniversalID: "3d2b5f28-4168-497a-a66f-9ed12fe54223",
		ForwardLabel: "colleague", ReverseLabel: "colleague", IsSymmetric: true, IsCanonical: true, RelatedType: "colleague"},
	{Slug: "co-resident", UniversalID: "e9942842-51d2-465b-9fbd-81ed348fdc2e",
		ForwardLabel: "co-resident", ReverseLabel: "co-resident", IsSymmetric: true, IsCanonical: true, RelatedType: "co-resident"},
	{Slug: "neighbor", UniversalID: "32a4e047-848f-4302-957a-4f3736ee29e9",
		ForwardLabel: "neighbor", ReverseLabel: "neighbor", IsSymmetric: true, IsCanonical: true, RelatedType: "neighbor"},
	{Slug: "sibling", UniversalID: "b94d3c6c-dd83-4861-a42e-3fc1e5e684df",
		ForwardLabel: "sibling", ReverseLabel: "sibling", IsSymmetric: true, IsCanonical: true, RelatedType: "sibling"},
	{Slug: "spouse", UniversalID: "9f055281-6064-40ea-86fc-6b1a5d620d0b",
		ForwardLabel: "spouse", ReverseLabel: "spouse", IsSymmetric: true, IsCanonical: true, RelatedType: "spouse"},
	{Slug: "kin", UniversalID: "8d29030b-cd28-4966-96d2-3beddbbc579a",
		ForwardLabel: "relative", ReverseLabel: "relative", IsSymmetric: true, IsCanonical: true, RelatedType: "kin",
		Description: "A family relative whose exact relation is unspecified."},
	{Slug: "date", UniversalID: "1f6c13b3-496a-4817-b416-6d07b8adb764",
		ForwardLabel: "date", ReverseLabel: "date", IsSymmetric: true, IsCanonical: true, RelatedType: "date"},
	{Slug: "sweetheart", UniversalID: "80cedce5-5e11-4dee-87ff-39846d97d674",
		ForwardLabel: "sweetheart", ReverseLabel: "sweetheart", IsSymmetric: true, IsCanonical: true, RelatedType: "sweetheart"},
	{Slug: "parent", UniversalID: "cb3869d4-683b-48e3-a30a-4e8f7e7dc7fb",
		ForwardLabel: "parent", ReverseLabel: "child", IsCanonical: true, InverseSlug: "child", RelatedType: "parent",
		Description: "The canonical orientation of the parent/child pair; a 'child' write is stored as 'parent' with the endpoints swapped."},
	{Slug: "child", UniversalID: "676d5466-8ecc-4cb9-a801-042e8ea87341",
		ForwardLabel: "child", ReverseLabel: "parent", IsCanonical: false, InverseSlug: "parent", RelatedType: "child",
		Description: "The non-canonical orientation of the parent/child pair, kept so RELATED;TYPE=child maps losslessly."},
	{Slug: "muse", UniversalID: "7202c876-95b3-42c1-920a-54586fc445f3",
		ForwardLabel: "muse", ReverseLabel: "inspired by", IsCanonical: true, RelatedType: "muse"},
	{Slug: "crush", UniversalID: "8eecd031-7f3c-4740-9290-46a8399e25d2",
		ForwardLabel: "crush", ReverseLabel: "admirer", IsCanonical: true, RelatedType: "crush"},
	{Slug: "agent", UniversalID: "6f2ee644-ba8d-414b-9134-a800701c1aca",
		ForwardLabel: "agent", ReverseLabel: "principal", IsCanonical: true, RelatedType: "agent"},
	{Slug: "emergency", UniversalID: "3f3916a0-de39-445d-96ee-7a242a4071f2",
		ForwardLabel: "emergency contact", ReverseLabel: "emergency contact for", IsCanonical: true, RelatedType: "emergency"},
}

// EnsureSeededRelationshipTypes inserts any missing system relationship type,
// repairs structural drift on existing ones, and links the one inverse pair.
//
// It runs on every InitSchema rather than behind an applied_migrations gate.
// That ledger is for one-time data migrations, and a run-once seed could never
// deliver an upstream correction to a database that already exists. It is also
// deliberately not raw INSERT ... ON CONFLICT DO NOTHING in the .sql files,
// because seeded rows would then bypass the validation that user-created rows
// go through.
//
// Reconciliation splits the columns into two classes:
//
//	Never written on re-seed (identity + user-owned presentation):
//	  universal_id, slug, forward_label, reverse_label, vcard_related_type,
//	  color, icon, description
//	Reconciled (structural, so an upstream fix reaches existing databases):
//	  is_symmetric, is_canonical, inverse_type_id, ownership, is_deletable
//
// forward_label and reverse_label are user-owned because relabelling is the
// point of having a mutable label; a user who renames "friend" to "mate" keeps
// that name across every future upgrade.
//
// vcard_related_type is also user-owned. A user may clear a seeded mapping and
// assign that registered RELATED TYPE to another type. Re-seeding must preserve
// that choice: writing the original mapping back can conflict with the unique
// index after the user has assigned it elsewhere.
//
// is_symmetric is safe to reconcile ONLY because it is immutable through the
// API (RelationshipTypeUpdate does not expose it), so reconciling can never
// undo a user's choice. Note the release-engineering consequence: changing a
// seeded type's symmetry in a future version would re-orient existing edges of
// that type and therefore needs a real data migration, not just a seed edit.
func (s *Store) EnsureSeededRelationshipTypes() error {
	return s.EnsureSeededRelationshipTypesContext(context.Background())
}

func (s *Store) EnsureSeededRelationshipTypesContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		inserted := make(map[string]bool, len(systemRelationshipTypes))
		for _, seed := range systemRelationshipTypes {
			didInsert, err := s.reconcileSeededRelationshipTypeTx(ctx, tx, seed)
			if err != nil {
				return err
			}
			inserted[seed.UniversalID] = didInsert
		}
		// Second pass: link inverse pairs now that both rows exist. Keyed on
		// universal_id like the first pass, and idempotent because it only
		// writes when the stored value differs.
		for _, seed := range systemRelationshipTypes {
			if seed.InverseSlug == "" {
				continue
			}
			if err := s.reconcileSeededInverseTypeTx(ctx, tx, seed, inserted[seed.UniversalID]); err != nil {
				return fmt.Errorf("link relationship type %q inverse: %w", seed.Slug, err)
			}
		}
		return nil
	})
}

// reconcileSeededRelationshipTypeTx inserts the seed when absent and otherwise
// updates only the structural columns, and only when one actually differs, so a
// no-op run bumps neither revision nor updated_at.
//
// The comparison is done in Go rather than as a SQL `WHERE a <> ? OR b <> ?`
// guard because `<>` against NULL yields NULL, so the nullable inverse_type_id
// column would silently never match and drift on that column would never be
// repaired.
func (s *Store) reconcileSeededRelationshipTypeTx(
	ctx context.Context, tx *loggedTx, seed systemRelationshipTypeSeed,
) (bool, error) {
	current, err := scanRelationshipType(tx.QueryRowContext(ctx,
		`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE universal_id = ?`,
		seed.UniversalID))
	if errors.Is(err, sql.ErrNoRows) {
		var description any
		if seed.Description != "" {
			description = seed.Description
		}
		var inverseTypeID any
		if !seed.IsCanonical {
			if err := tx.QueryRowContext(ctx,
				`SELECT id FROM relationship_types WHERE slug = ?`, seed.InverseSlug,
			).Scan(&inverseTypeID); err != nil {
				return false, fmt.Errorf("load inverse relationship type %q for %q: %w",
					seed.InverseSlug, seed.Slug, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO relationship_types (
				universal_id, slug, forward_label, reverse_label,
				is_symmetric, is_canonical, inverse_type_id, vcard_related_type, description,
				ownership, is_deletable
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, FALSE)
		`,
			seed.UniversalID, seed.Slug, seed.ForwardLabel, seed.ReverseLabel,
			seed.IsSymmetric, seed.IsCanonical, inverseTypeID, seed.RelatedType, description,
			string(RelationshipTypeOwnershipSystem),
		); err != nil {
			return false, fmt.Errorf("seed relationship type %q: %w", seed.Slug, err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("load seeded relationship type %q: %w", seed.Slug, err)
	}
	if !seededRelationshipTypeDiffers(current, seed) {
		return false, nil
	}
	if !seed.IsCanonical {
		var inverseTypeID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM relationship_types WHERE slug = ?`, seed.InverseSlug,
		).Scan(&inverseTypeID); err != nil {
			return false, fmt.Errorf("load inverse relationship type %q for %q: %w",
				seed.InverseSlug, seed.Slug, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE relationship_types
			SET is_symmetric = ?, is_canonical = ?, inverse_type_id = ?,
			    ownership = ?, is_deletable = FALSE,
			    revision = revision + 1, updated_at = %s
			WHERE universal_id = ?
		`, s.dialect.Now()),
			seed.IsSymmetric, seed.IsCanonical, inverseTypeID,
			string(RelationshipTypeOwnershipSystem), seed.UniversalID,
		); err != nil {
			return false, fmt.Errorf("repair seeded relationship type %q: %w", seed.Slug, err)
		}
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE relationship_types
		SET is_symmetric = ?, is_canonical = ?,
		    ownership = ?, is_deletable = FALSE,
		    revision = revision + 1, updated_at = %s
		WHERE universal_id = ?
	`, s.dialect.Now()),
		seed.IsSymmetric, seed.IsCanonical,
		string(RelationshipTypeOwnershipSystem), seed.UniversalID,
	); err != nil {
		return false, fmt.Errorf("repair seeded relationship type %q: %w", seed.Slug, err)
	}
	return false, nil
}

// reconcileSeededInverseTypeTx repairs an inverse link and records a real
// mutation. New seed rows keep their initial revision: their inverse link is
// part of the initial seed construction, not a later reconciliation update.
func (s *Store) reconcileSeededInverseTypeTx(
	ctx context.Context, tx *loggedTx, seed systemRelationshipTypeSeed, inserted bool,
) error {
	if inserted {
		_, err := tx.ExecContext(ctx, `
			UPDATE relationship_types
			SET inverse_type_id = (
				SELECT inverse.id FROM relationship_types inverse WHERE inverse.slug = ?
			)
			WHERE universal_id = ?
			  AND (inverse_type_id IS NULL
			       OR inverse_type_id <> (
			           SELECT inverse.id FROM relationship_types inverse WHERE inverse.slug = ?
			       ))
		`, seed.InverseSlug, seed.UniversalID, seed.InverseSlug)
		return err
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE relationship_types
		SET inverse_type_id = (
				SELECT inverse.id FROM relationship_types inverse WHERE inverse.slug = ?
			), revision = revision + 1, updated_at = %s
		WHERE universal_id = ?
		  AND (inverse_type_id IS NULL
		       OR inverse_type_id <> (
		           SELECT inverse.id FROM relationship_types inverse WHERE inverse.slug = ?
		       ))
	`, s.dialect.Now()), seed.InverseSlug, seed.UniversalID, seed.InverseSlug)
	return err
}

// seededRelationshipTypeDiffers reports whether any reconciled column has
// drifted from the seed. It deliberately ignores slug, labels, vCard mapping,
// colour, icon, and description, which the seed never rewrites.
func seededRelationshipTypeDiffers(
	current *RelationshipType, seed systemRelationshipTypeSeed,
) bool {
	if current.IsSymmetric != seed.IsSymmetric ||
		current.IsCanonical != seed.IsCanonical ||
		current.Ownership != RelationshipTypeOwnershipSystem ||
		current.IsDeletable {
		return true
	}
	return false
}
