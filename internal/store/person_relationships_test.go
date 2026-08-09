package store_test

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// mustTwoPersons promotes two synthetic participants to durable persons and
// returns their person IDs in creation order.
func mustTwoPersons(t *testing.T, f *storetest.Fixture) (int64, int64) {
	t.Helper()
	aliceParticipant := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	bobParticipant := f.EnsureParticipant("bob@example.com", "bob", "example.com")
	alice, _, err := f.Store.CreatePersonFromParticipant(aliceParticipant)
	require.NoError(t, err)
	bob, _, err := f.Store.CreatePersonFromParticipant(bobParticipant)
	require.NoError(t, err)
	return alice.ID, bob.ID
}

func TestAddPersonRelationshipCanonicalizesOrientationAndSymmetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)

	group, propID, altID := "item1", "prop-1", "alt-1"
	parent, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "parent",
		Source: store.ProvenanceUser, Actor: "user",
		VCardIdentity: store.VCardIdentity{
			Property: "RELATED", Group: &group, PropID: &propID, PID: []string{"1.1", "2.1"}, AltID: &altID,
		},
	})
	require.NoError(err)
	assert.Equal(alice, parent.SourcePersonID)
	assert.Equal(bob, parent.TargetPersonID)
	assert.Equal("parent", parent.TypeSlug)
	assert.Equal("parent", parent.ForwardLabel)
	assert.Equal("child", parent.ReverseLabel)
	assert.False(parent.IsSymmetric)
	assert.Equal(store.RelationshipStatusActive, parent.Status)
	assert.Equal(int64(1), parent.Revision)
	assert.Equal("RELATED", parent.VCardIdentity.Property)
	require.NotNil(parent.VCardIdentity.Group)
	assert.Equal(group, *parent.VCardIdentity.Group)
	assert.Equal([]string{"1.1", "2.1"}, parent.VCardIdentity.PID)

	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: bob, TargetPersonID: alice, TypeSlug: "child",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrPersonRelationshipDuplicate)

	lower, higher := alice, bob
	if lower > higher {
		lower, higher = higher, lower
	}
	spouse, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: higher, TargetPersonID: lower, TypeSlug: "spouse",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	assert.Equal(lower, spouse.SourcePersonID)
	assert.Equal(higher, spouse.TargetPersonID)
	assert.True(spouse.IsSymmetric)
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: lower, TargetPersonID: higher, TypeSlug: "spouse",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrPersonRelationshipDuplicate)

	forward, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "agent",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	reverse, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: bob, TargetPersonID: alice, TypeSlug: "agent",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	assert.NotEqual(forward.ID, reverse.ID)
}

func TestPersonRelationshipValidatesInputAndConstructedDateBounds(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)

	_, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: alice, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrPersonRelationshipSelf)
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: 999999, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrPersonNotFound)
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "missing", Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrRelationshipTypeNotFound)

	yearless := store.PartialDate{Month: new(4), Day: new(12)}
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "friend", StartDate: &yearless,
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrInvalidPartialDate)
	invalid := store.PartialDate{Year: new(2020), Month: new(13)}
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "friend", StartDate: &invalid,
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrInvalidPartialDate)

	from, err := store.ParseRelationshipDate("2020-06")
	require.NoError(err)
	until, err := store.ParseRelationshipDate("2019")
	require.NoError(err)
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "friend", StartDate: &from, EndDate: &until,
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.ErrorIs(err, store.ErrPersonRelationshipInterval)
	confidence := 0.5
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "friend", Source: store.ProvenanceUser,
		Confidence: &confidence, Actor: "user",
	})
	require.ErrorIs(err, store.ErrConfidenceScope)
	notANumber := math.NaN()
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "acquaintance", Source: store.ProvenanceEnrichment,
		Confidence: &notANumber, Actor: "system",
	})
	require.ErrorIs(err, store.ErrConfidenceScope, "NaN is not an in-range confidence")
}

func TestEndUpdateDeletePersonRelationshipUsesCASAndKeepsHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)
	edge, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "spouse", Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	yearless := store.PartialDate{Month: new(4), Day: new(12)}
	_, err = f.Store.EndPersonRelationshipContext(ctx, edge.ID, edge.Revision, yearless, "user")
	require.ErrorIs(err, store.ErrInvalidPartialDate)
	invalid := store.PartialDate{Year: new(2020), Month: new(13)}
	_, err = f.Store.EndPersonRelationshipContext(ctx, edge.ID, edge.Revision, invalid, "user")
	require.ErrorIs(err, store.ErrInvalidPartialDate)

	until, err := store.ParseRelationshipDate("2021-08")
	require.NoError(err)
	ended, err := f.Store.EndPersonRelationshipContext(ctx, edge.ID, edge.Revision, until, "user")
	require.NoError(err)
	assert.Equal(store.RelationshipStatusEnded, ended.Status)
	require.NotNil(ended.EndDate)
	assert.Equal("2021-08", ended.EndDate.String())
	reestablished, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "spouse", Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	assert.NotEqual(edge.ID, reestablished.ID, "ending preserves history and frees the active slot")
	_, err = f.Store.EndPersonRelationshipContext(ctx, edge.ID, edge.Revision, until, "user")
	require.ErrorIs(err, store.ErrPersonRelationshipRevisionConflict)

	notes := "  met at the block party  "
	updated, err := f.Store.UpdatePersonRelationshipNotesContext(ctx, edge.ID, ended.Revision, &notes, "user")
	require.NoError(err)
	require.NotNil(updated.Notes)
	assert.Equal("met at the block party", *updated.Notes)
	require.ErrorIs(f.Store.DeletePersonRelationshipContext(ctx, edge.ID, ended.Revision), store.ErrPersonRelationshipRevisionConflict)
	require.NoError(f.Store.DeletePersonRelationshipContext(ctx, edge.ID, updated.Revision))
	_, err = f.Store.GetPersonRelationshipContext(ctx, edge.ID)
	require.ErrorIs(err, store.ErrPersonRelationshipNotFound)
}

func TestPatchPersonRelationshipIsAtomicAndBumpsRevisionOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)
	edge, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "spouse", Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	until, err := store.ParseRelationshipDate("2021-08")
	require.NoError(err)
	tooLong := strings.Repeat("x", 4097)
	_, err = f.Store.PatchPersonRelationshipContext(ctx, edge.ID, edge.Revision,
		store.PersonRelationshipPatch{EndDate: &until, UpdateEndDate: true, Notes: &tooLong, UpdateNotes: true}, "user")
	require.ErrorIs(err, store.ErrPersonRelationshipInvalid)
	unchanged, err := f.Store.GetPersonRelationshipContext(ctx, edge.ID)
	require.NoError(err)
	assert.Nil(unchanged.EndDate)
	assert.Nil(unchanged.Notes)
	assert.Equal(edge.Revision, unchanged.Revision)

	notes := "  met at the block party  "
	updated, err := f.Store.PatchPersonRelationshipContext(ctx, edge.ID, edge.Revision,
		store.PersonRelationshipPatch{EndDate: &until, UpdateEndDate: true, Notes: &notes, UpdateNotes: true}, "user")
	require.NoError(err)
	require.NotNil(updated.EndDate)
	require.NotNil(updated.Notes)
	assert.Equal("2021-08", updated.EndDate.String())
	assert.Equal("met at the block party", *updated.Notes)
	assert.Equal(edge.Revision+1, updated.Revision)
}

// TestPersonRelationshipConstraintsAreEnforcedByTheDatabase deliberately
// writes through Store.DB(), bypassing Go validation, so each portable CHECK
// is exercised on both SQLite and PostgreSQL rather than merely asserted at a
// store boundary.
func TestPersonRelationshipConstraintsAreEnforcedByTheDatabase(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)
	insert := f.Store.Rebind(`
		INSERT INTO person_relationships (
			source_person_id, target_person_id, relationship_type_id,
			start_year, start_month, start_day, end_year, end_month, end_day,
			status, source, confidence, created_by, updated_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'user', 'user')
	`)
	tests := []struct {
		name                            string
		typeSlug                        string
		source, target                  int64
		startYear, startMonth, startDay any
		endYear, endMonth, endDay       any
		provenance, confidence          any
	}{
		{"self edge", "contact", alice, alice, nil, nil, nil, nil, nil, nil, "user", nil},
		{"start month without year", "acquaintance", alice, bob, nil, 4, nil, nil, nil, nil, "user", nil},
		{"start day without month", "friend", alice, bob, 2019, nil, 12, nil, nil, nil, "user", nil},
		{"start month range", "met", alice, bob, 2019, 13, nil, nil, nil, nil, "user", nil},
		{"start year range", "co-worker", alice, bob, 0, nil, nil, nil, nil, nil, "user", nil},
		{"start day range", "colleague", alice, bob, 2019, 4, 32, nil, nil, nil, "user", nil},
		{"end month without year", "co-resident", alice, bob, nil, nil, nil, nil, 6, nil, "user", nil},
		{"end day without month", "neighbor", alice, bob, nil, nil, nil, 2019, nil, 12, "user", nil},
		{"end month range", "sibling", alice, bob, nil, nil, nil, 2019, 13, nil, "user", nil},
		{"end year range", "spouse", alice, bob, nil, nil, nil, 0, nil, nil, "user", nil},
		{"end day range", "kin", alice, bob, nil, nil, nil, 2019, 4, 32, "user", nil},
		{"bad provenance", "date", alice, bob, nil, nil, nil, nil, nil, nil, "guess", nil},
		{"declared confidence", "sweetheart", alice, bob, nil, nil, nil, nil, nil, nil, "user", 0.5},
		{"confidence range", "parent", alice, bob, nil, nil, nil, nil, nil, nil, "enrichment", 1.5},
	}
	require.Len(tests, 14)
	seen := make(map[string]struct{}, len(tests))
	for _, test := range tests {
		seen[test.typeSlug] = struct{}{}
	}
	require.Len(seen, len(tests), "distinct types prevent a unique-index error from masking a CHECK")
	for _, test := range tests {
		relationshipType, err := f.Store.GetRelationshipTypeBySlugContext(ctx, test.typeSlug)
		require.NoError(err)
		_, err = f.Store.DB().Exec(insert,
			test.source, test.target, relationshipType.ID,
			test.startYear, test.startMonth, test.startDay, test.endYear, test.endMonth, test.endDay,
			"active", test.provenance, test.confidence,
		)
		require.Errorf(err, "database must reject %s", test.name)
	}
	accepted, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "muse")
	require.NoError(err)
	_, err = f.Store.DB().Exec(insert,
		alice, bob, accepted.ID, 2019, 4, 12, nil, nil, nil, "active", "user", nil,
	)
	require.NoError(err)
}

func TestPersonRelationshipConstraintsAndColumnParity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)
	friend, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO person_relationships (source_person_id, target_person_id, relationship_type_id, start_month, status, source, created_by, updated_by)
		VALUES (?, ?, ?, 4, 'active', 'user', 'user', 'user')`), alice, bob, friend.ID)
	require.Error(err, "database must reject a relationship bound without a year")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO person_relationships (source_person_id, target_person_id, relationship_type_id, status, source, confidence, created_by, updated_by)
		VALUES (?, ?, ?, 'active', 'user', 0.5, 'user', 'user')`), alice, bob, friend.ID)
	require.Error(err, "database must reject confidence on a declared source")

	columnsForTable := func(table string) []string {
		rows, queryErr := f.Store.DB().Query(`SELECT * FROM ` + table + ` WHERE 1 = 0`)
		require.NoError(queryErr)
		defer func() { require.NoError(rows.Close()) }()
		columns, queryErr := rows.Columns()
		require.NoError(queryErr)
		require.NoError(rows.Err())
		slices.Sort(columns)
		return columns
	}
	for table, want := range map[string][]string{
		"relationship_types":          {"color", "created_at", "description", "forward_label", "icon", "id", "inverse_type_id", "is_canonical", "is_deletable", "is_symmetric", "ownership", "reverse_label", "revision", "slug", "universal_id", "updated_at", "vcard_related_type"},
		"person_relationships":        {"confidence", "created_at", "created_by", "end_day", "end_month", "end_year", "id", "notes", "relationship_type_id", "revision", "source", "source_person_id", "source_ref", "start_day", "start_month", "start_year", "status", "target_person_id", "updated_at", "updated_by", "vcard_altid", "vcard_group", "vcard_pid", "vcard_prop_id", "vcard_property"},
		"person_relationship_reviews": {"accepted_relationship_id", "created_at", "created_by", "id", "matched_person_id", "person_id", "raw_related_type", "raw_related_value", "reviewed_at", "reviewed_by", "source", "source_ref", "status", "updated_at", "value_kind", "vcard_altid", "vcard_group", "vcard_pid", "vcard_prop_id", "vcard_property"},
	} {
		assert.Equal(want, columnsForTable(table))
	}
}

func TestDeletingAPersonCascadesItsRelationships(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)
	edge, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	person, err := f.Store.GetPerson(bob)
	require.NoError(err)
	require.NoError(f.Store.DeletePerson(bob, person.Revision))
	_, err = f.Store.GetPersonRelationshipContext(ctx, edge.ID)
	require.ErrorIs(err, store.ErrPersonRelationshipNotFound)
}
