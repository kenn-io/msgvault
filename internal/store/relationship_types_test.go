package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSeededRelationshipTypesCoverEveryRegisteredRelatedValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	types, err := f.Store.ListRelationshipTypesContext(context.Background())
	require.NoError(err)
	require.Len(types, 19)

	mapped := make(map[string]store.RelationshipType, len(types))
	for _, relationshipType := range types {
		assert.Equal(store.RelationshipTypeOwnershipSystem, relationshipType.Ownership,
			"seeded types are system-owned")
		assert.False(relationshipType.IsDeletable, "seeded types are not deletable")
		assert.Equal(int64(1), relationshipType.Revision)
		// Universal IDs are opaque hardcoded UUIDs, never derived from the
		// slug, so identity and machine name stay independent.
		assert.Regexp(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`,
			relationshipType.UniversalID)
		assert.NotContains(relationshipType.UniversalID, relationshipType.Slug,
			"a seeded universal ID must not embed its slug")
		require.NotNil(relationshipType.VCardRelatedType)
		mapped[*relationshipType.VCardRelatedType] = relationshipType
	}

	// Universal IDs are stable identity: they must be unique and must match
	// the hardcoded seed table exactly, because an existing database is
	// reconciled by universal_id. Changing one would orphan its row.
	seen := make(map[string]string, len(types))
	for _, relationshipType := range types {
		previous, duplicate := seen[relationshipType.UniversalID]
		assert.Falsef(duplicate, "universal ID %q is shared by %q and %q",
			relationshipType.UniversalID, previous, relationshipType.Slug)
		seen[relationshipType.UniversalID] = relationshipType.Slug
	}
	assert.Len(seen, 19)

	// Every registered RELATED TYPE value is accounted for: either it is
	// seeded, or it is in the explicit unmapped set with a reason.
	unmapped := store.UnmappedRelatedTypes()
	assert.Len(unmapped, 1)
	assert.Contains(unmapped, "me")
	assert.NotEmpty(unmapped["me"], "an unmapped value must document why")

	registered := store.RelatedTypeValues()
	assert.Len(registered, 20)
	for _, value := range registered {
		_, seeded := mapped[value]
		_, skipped := unmapped[value]
		assert.NotEqualf(seeded, skipped,
			"RELATED TYPE %q must be exactly one of seeded or explicitly unmapped", value)
	}
	assert.Len(mapped, 19)
}

func TestSeededRelationshipTypeOrientationAndSymmetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	parent, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "parent")
	require.NoError(err)
	assert.Equal("parent", parent.ForwardLabel)
	assert.Equal("child", parent.ReverseLabel)
	assert.False(parent.IsSymmetric)
	assert.True(parent.IsCanonical)

	child, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "child")
	require.NoError(err)
	assert.Equal("child", child.ForwardLabel)
	assert.Equal("parent", child.ReverseLabel)
	assert.False(child.IsSymmetric)
	assert.False(child.IsCanonical, "child stores as its canonical inverse, parent")
	require.NotNil(child.InverseTypeID)
	assert.Equal(parent.ID, *child.InverseTypeID)
	require.NotNil(parent.InverseTypeID)
	assert.Equal(child.ID, *parent.InverseTypeID)

	// No gendered parent variants are seeded, and neither are the two
	// context-qualifying TYPE values ("work", "home") that RFC 6350's shared
	// multi-property parameter row also allows on RELATED.
	for _, slug := range []string{"father", "mother", "son", "daughter", "work", "home"} {
		_, err := f.Store.GetRelationshipTypeBySlugContext(ctx, slug)
		require.ErrorIsf(err, store.ErrRelationshipTypeNotFound, "slug %q must not be seeded", slug)
	}
	for _, contextual := range []string{"work", "home"} {
		assert.NotContainsf(store.RelatedTypeValues(), contextual,
			"%q qualifies a relation's context and is outside the relationship value set", contextual)
	}

	spouse, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "spouse")
	require.NoError(err)
	assert.True(spouse.IsSymmetric)
	assert.Equal(spouse.ForwardLabel, spouse.ReverseLabel)
	assert.Nil(spouse.InverseTypeID, "a symmetric type is its own inverse")
}

func TestEnsureSeededRelationshipTypesIsIdempotentAndPreservesLabelEdits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	friend, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)

	renamed := "mate"
	updated, err := f.Store.UpdateRelationshipTypeContext(ctx, friend.ID, friend.Revision,
		store.RelationshipTypeUpdate{ForwardLabel: &renamed, ReverseLabel: &renamed})
	require.NoError(err)
	assert.Equal("mate", updated.ForwardLabel)

	// Corrupt a structural column the way a stale database or a bad manual
	// edit would, so the reconciler has something real to repair.
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE relationship_types SET is_symmetric = FALSE, is_deletable = TRUE WHERE slug = ?`), "friend")
	require.NoError(err)

	require.NoError(f.Store.EnsureSeededRelationshipTypes())

	after, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)
	// User-owned columns survive; structural drift is repaired.
	assert.Equal("mate", after.ForwardLabel, "re-seeding must not overwrite a user's label edit")
	assert.Equal("mate", after.ReverseLabel)
	assert.Equal(friend.UniversalID, after.UniversalID, "universal ID is identity, never rewritten")
	assert.Equal("friend", after.Slug, "slug is immutable")
	assert.True(after.IsSymmetric, "structural drift must be repaired")
	require.NotNil(after.VCardRelatedType)
	assert.Equal("friend", *after.VCardRelatedType)
	assert.False(after.IsDeletable)
	assert.Equal(updated.Revision+1, after.Revision, "one repair advances the revision once")

	// A second run finds nothing to do, so it must not bump the revision.
	require.NoError(f.Store.EnsureSeededRelationshipTypes())
	settled, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)
	assert.Equal(after.Revision, settled.Revision, "a no-op reconcile must not write")
	assert.Equal(after.UpdatedAt, settled.UpdatedAt)

	types, err := f.Store.ListRelationshipTypesContext(ctx)
	require.NoError(err)
	assert.Len(types, 19, "re-seeding must not duplicate rows")
}

func TestInitSchemaPreservesUserRemappedSeededVCardType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	friend, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)

	// A user may release a seeded mapping and assign the registered RELATED
	// TYPE to a user-owned type. The unique index then prevents the old seed
	// value from being restored on the next startup.
	cleared := ""
	clearedFriend, err := f.Store.UpdateRelationshipTypeContext(ctx, friend.ID, friend.Revision,
		store.RelationshipTypeUpdate{VCardRelatedType: &cleared})
	require.NoError(err)
	assert.Nil(clearedFriend.VCardRelatedType)

	relatedType := "friend"
	alias, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "friend-alias", ForwardLabel: "friend alias", ReverseLabel: "friend alias",
		IsSymmetric: true, VCardRelatedType: &relatedType,
	})
	require.NoError(err)

	// InitSchema is the production startup path. It must not restore the
	// original seed mapping or fail on the unique constraint.
	require.NoError(f.Store.InitSchema(), "a later startup must preserve the remapping")

	friendAfter, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)
	assert.Nil(friendAfter.VCardRelatedType, "clearing a seeded mapping must persist")
	aliasAfter, err := f.Store.GetRelationshipTypeContext(ctx, alias.ID)
	require.NoError(err)
	require.NotNil(aliasAfter.VCardRelatedType)
	assert.Equal("friend", *aliasAfter.VCardRelatedType)

	// The remapped TYPE still resolves through the production RELATED import
	// path, so preserving the user choice keeps its intended semantics.
	aliceID, bobID := mustTwoPersons(t, f)
	bob, err := f.Store.GetPerson(bobID)
	require.NoError(err)
	resolution, err := f.Store.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: aliceID, RawValue: bob.VCardUID, RawType: "friend",
		ValueKind: store.RelatedValueKindURI, Source: store.ProvenanceVCardImport, Actor: "system",
	})
	require.NoError(err)
	require.NotNil(resolution.Relationship)
	assert.Equal(alias.ID, resolution.Relationship.RelationshipTypeID)
	assert.Equal("friend-alias", resolution.Relationship.TypeSlug)
}

func TestEnsureSeededRelationshipTypesRestoresADeletedSeed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	// A partially seeded or hand-edited database self-heals, which is why the
	// reconciler runs on every open instead of behind an applied_migrations
	// gate that can only ever fire once.
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`DELETE FROM relationship_types WHERE slug = ?`), "neighbor")
	require.NoError(err)

	require.NoError(f.Store.EnsureSeededRelationshipTypes())

	restored, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "neighbor")
	require.NoError(err)
	assert.Equal("neighbor", restored.ForwardLabel)
	assert.Equal(store.RelationshipTypeOwnershipSystem, restored.Ownership)
	assert.False(restored.IsDeletable)

	types, err := f.Store.ListRelationshipTypesContext(ctx)
	require.NoError(err)
	assert.Len(types, 19)
}

func TestEnsureSeededRelationshipTypesRepairsStructuralDriftButPreservesLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	friend, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)

	// Labels and vCard mappings are mutable presentation/interchange metadata,
	// while these three fields are structural.
	// The stale timestamp makes a successful repair observable on SQLite too,
	// whose datetime('now') has second precision.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE relationship_types
		SET forward_label = ?, reverse_label = ?, is_symmetric = FALSE,
		    is_deletable = TRUE,
		    updated_at = '2000-01-01 00:00:00'
		WHERE id = ?
	`), "mate", "mate", friend.ID)
	require.NoError(err)

	corrupted, err := f.Store.GetRelationshipTypeContext(ctx, friend.ID)
	require.NoError(err)
	require.False(corrupted.IsSymmetric)
	require.NotNil(corrupted.VCardRelatedType)
	assert.Equal("friend", *corrupted.VCardRelatedType)
	require.True(corrupted.IsDeletable)

	require.NoError(f.Store.EnsureSeededRelationshipTypes())

	repaired, err := f.Store.GetRelationshipTypeContext(ctx, friend.ID)
	require.NoError(err)
	assert.Equal("mate", repaired.ForwardLabel)
	assert.Equal("mate", repaired.ReverseLabel)
	assert.True(repaired.IsSymmetric)
	require.NotNil(repaired.VCardRelatedType)
	assert.Equal("friend", *repaired.VCardRelatedType)
	assert.False(repaired.IsDeletable)
	assert.Equal(corrupted.Revision+1, repaired.Revision)
	assert.NotEqual(corrupted.UpdatedAt, repaired.UpdatedAt)

	require.NoError(f.Store.EnsureSeededRelationshipTypes())
	settled, err := f.Store.GetRelationshipTypeContext(ctx, friend.ID)
	require.NoError(err)
	assert.Equal(repaired.Revision, settled.Revision)
	assert.Equal(repaired.UpdatedAt, settled.UpdatedAt)
}

func TestEnsureSeededRelationshipTypesRepairsInverseDriftWithRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	parent, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "parent")
	require.NoError(err)
	child, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "child")
	require.NoError(err)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE relationship_types
		SET inverse_type_id = NULL, updated_at = '2000-01-01 00:00:00'
		WHERE id = ?
	`), parent.ID)
	require.NoError(err)

	corrupted, err := f.Store.GetRelationshipTypeContext(ctx, parent.ID)
	require.NoError(err)
	require.Nil(corrupted.InverseTypeID)

	require.NoError(f.Store.EnsureSeededRelationshipTypes())

	repaired, err := f.Store.GetRelationshipTypeContext(ctx, parent.ID)
	require.NoError(err)
	require.NotNil(repaired.InverseTypeID)
	assert.Equal(child.ID, *repaired.InverseTypeID)
	assert.Equal(corrupted.Revision+1, repaired.Revision)
	assert.NotEqual(corrupted.UpdatedAt, repaired.UpdatedAt)

	require.NoError(f.Store.EnsureSeededRelationshipTypes())
	settled, err := f.Store.GetRelationshipTypeContext(ctx, parent.ID)
	require.NoError(err)
	assert.Equal(repaired.Revision, settled.Revision)
	assert.Equal(repaired.UpdatedAt, settled.UpdatedAt)
}

func TestCreateRelationshipTypeAssignsUUIDUniversalIDAndUserOwnership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	color := "#336699"
	created, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug:         "mentor",
		ForwardLabel: "mentor",
		ReverseLabel: "mentee",
		Color:        &color,
	})
	require.NoError(err)
	assert.Equal("mentor", created.Slug)
	assert.Equal("mentor", created.ForwardLabel)
	assert.Equal("mentee", created.ReverseLabel)
	assert.False(created.IsSymmetric)
	assert.True(created.IsCanonical, "user types are always their own canonical orientation")
	assert.Nil(created.InverseTypeID)
	assert.Equal(store.RelationshipTypeOwnershipUser, created.Ownership)
	assert.True(created.IsDeletable)
	assert.Equal(int64(1), created.Revision)
	assert.Nil(created.VCardRelatedType)
	require.NotNil(created.Color)
	assert.Equal("#336699", *created.Color)
	// A user type gets an opaque random universal ID, never the reserved
	// deterministic prefix used by seeded types.
	assert.Regexp(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, created.UniversalID)

	_, err = f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "mentor", ForwardLabel: "guide", ReverseLabel: "guided",
	})
	require.ErrorIs(err, store.ErrRelationshipTypeSlugConflict)

	_, err = f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "friend", ForwardLabel: "friend", ReverseLabel: "friend", IsSymmetric: true,
	})
	require.ErrorIs(err, store.ErrRelationshipTypeSlugConflict,
		"a user type must not shadow a seeded slug")
}

func TestCreateRelationshipTypeValidatesSlugSymmetryAndRelatedType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	for _, slug := range []string{
		"", "  ", "Mentor", "co worker", "co_worker", "-lead", "lead-",
		"co--worker", "über", "a/b",
	} {
		_, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
			Slug: slug, ForwardLabel: "a", ReverseLabel: "b",
		})
		require.ErrorIsf(err, store.ErrRelationshipTypeInvalid, "slug %q must be rejected", slug)
	}

	_, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "buddy", ForwardLabel: "buddy", ReverseLabel: "pal", IsSymmetric: true,
	})
	require.ErrorIs(err, store.ErrRelationshipTypeSymmetricLabels)

	unregistered := "best-friend-forever"
	_, err = f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "bff", ForwardLabel: "bff", ReverseLabel: "bff", IsSymmetric: true,
		VCardRelatedType: &unregistered,
	})
	require.ErrorIs(err, store.ErrRelationshipTypeInvalid,
		"vcard_related_type must be a registered RELATED TYPE value")

	// "work" and "home" are legal TYPE values on RELATED via RFC 6350's
	// shared multi-property parameter row, but they qualify the context of a
	// relation rather than naming one, so they cannot be a relationship type's
	// vCard mapping. See relatedTypeValues for the full rationale.
	for _, contextual := range []string{"work", "home"} {
		value := contextual
		_, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
			Slug: "at-" + value, ForwardLabel: value, ReverseLabel: value, IsSymmetric: true,
			VCardRelatedType: &value,
		})
		require.ErrorIsf(err, store.ErrRelationshipTypeInvalid,
			"%q qualifies a relation's context and must not name a relationship type", value)
	}

	taken := "friend"
	_, err = f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "pal", ForwardLabel: "pal", ReverseLabel: "pal", IsSymmetric: true,
		VCardRelatedType: &taken,
	})
	require.ErrorIs(err, store.ErrRelationshipTypeRelatedTypeConflict)

	// 'me' is registered but deliberately unseeded, so it is still available.
	free := "me"
	created, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "same-person", ForwardLabel: "same person", ReverseLabel: "same person",
		IsSymmetric: true, VCardRelatedType: &free,
	})
	require.NoError(err)
	assert.Equal("me", *created.VCardRelatedType)
}

func TestUpdateRelationshipTypeEditsPresentationAndRejectsStaleRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	created, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "mentor", ForwardLabel: "mentor", ReverseLabel: "mentee",
	})
	require.NoError(err)

	forward := "coach"
	icon := "whistle"
	updated, err := f.Store.UpdateRelationshipTypeContext(ctx, created.ID, created.Revision,
		store.RelationshipTypeUpdate{ForwardLabel: &forward, Icon: &icon})
	require.NoError(err)
	assert.Equal("coach", updated.ForwardLabel)
	assert.Equal("mentee", updated.ReverseLabel, "an unset field is left unchanged")
	require.NotNil(updated.Icon)
	assert.Equal("whistle", *updated.Icon)
	assert.Equal(created.Revision+1, updated.Revision)
	assert.Equal(created.Slug, updated.Slug, "slug is immutable")
	assert.Equal(created.UniversalID, updated.UniversalID, "universal ID is immutable")

	cleared := ""
	afterClear, err := f.Store.UpdateRelationshipTypeContext(ctx, updated.ID, updated.Revision,
		store.RelationshipTypeUpdate{Icon: &cleared})
	require.NoError(err)
	assert.Nil(afterClear.Icon, "a pointer to the empty string clears the value")

	_, err = f.Store.UpdateRelationshipTypeContext(ctx, created.ID, created.Revision,
		store.RelationshipTypeUpdate{ForwardLabel: &forward})
	require.ErrorIs(err, store.ErrRelationshipTypeRevisionConflict)

	_, err = f.Store.UpdateRelationshipTypeContext(ctx, 999999, 1,
		store.RelationshipTypeUpdate{ForwardLabel: &forward})
	require.ErrorIs(err, store.ErrRelationshipTypeNotFound)
}

func TestRelationshipTypeVCardMappingNormalizesAndClears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	rawRelatedType := "  ME  "

	created, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "same-person", ForwardLabel: "same person", ReverseLabel: "same person",
		IsSymmetric: true, VCardRelatedType: &rawRelatedType,
	})
	require.NoError(err)
	require.NotNil(created.VCardRelatedType)
	assert.Equal("me", *created.VCardRelatedType)

	clearedMapping := ""
	updated, err := f.Store.UpdateRelationshipTypeContext(ctx, created.ID, created.Revision,
		store.RelationshipTypeUpdate{VCardRelatedType: &clearedMapping})
	require.NoError(err)
	assert.Nil(updated.VCardRelatedType)
}

func TestUpdateRelationshipTypeKeepsSymmetricLabelsIdentical(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	spouse, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "spouse")
	require.NoError(err)

	forwardOnly := "partner"
	_, err = f.Store.UpdateRelationshipTypeContext(ctx, spouse.ID, spouse.Revision,
		store.RelationshipTypeUpdate{ForwardLabel: &forwardOnly})
	require.ErrorIs(err, store.ErrRelationshipTypeSymmetricLabels,
		"editing one label of a symmetric type would make the two directions disagree")

	updated, err := f.Store.UpdateRelationshipTypeContext(ctx, spouse.ID, spouse.Revision,
		store.RelationshipTypeUpdate{ForwardLabel: &forwardOnly, ReverseLabel: &forwardOnly})
	require.NoError(err)
	assert.Equal("partner", updated.ForwardLabel)
	assert.Equal("partner", updated.ReverseLabel)
}

func TestDeleteRelationshipTypeProtectsSystemTypesAndTypesInUse(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	friend, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)
	require.ErrorIs(f.Store.DeleteRelationshipTypeContext(ctx, friend.ID, friend.Revision), store.ErrRelationshipTypeNotDeletable)

	created, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{Slug: "mentor", ForwardLabel: "mentor", ReverseLabel: "mentee"})
	require.NoError(err)
	alice, bob := mustTwoPersons(t, f)
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "mentor", Source: store.ProvenanceUser, Actor: "user"})
	require.NoError(err)
	require.ErrorIs(f.Store.DeleteRelationshipTypeContext(ctx, created.ID, created.Revision), store.ErrRelationshipTypeInUse)

	unused, err := f.Store.CreateRelationshipTypeContext(ctx, store.RelationshipTypeInput{
		Slug: "rival", ForwardLabel: "rival", ReverseLabel: "rival", IsSymmetric: true,
	})
	require.NoError(err)
	require.ErrorIs(f.Store.DeleteRelationshipTypeContext(ctx, unused.ID, unused.Revision-1), store.ErrRelationshipTypeRevisionConflict)
	require.NoError(f.Store.DeleteRelationshipTypeContext(ctx, unused.ID, unused.Revision))
	_, err = f.Store.GetRelationshipTypeContext(ctx, unused.ID)
	require.ErrorIs(err, store.ErrRelationshipTypeNotFound)
}
