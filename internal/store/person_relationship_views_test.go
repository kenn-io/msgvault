package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func mustPerson(t *testing.T, f *storetest.Fixture, email, username string) int64 {
	t.Helper()
	participant := f.EnsureParticipant(email, username, "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	_, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, &username)
	require.NoError(t, err)
	return person.ID
}

func mustSetPersonDisplayName(t *testing.T, f *storetest.Fixture, personID int64, displayName string) {
	t.Helper()
	person, err := f.Store.GetPersonContext(t.Context(), personID)
	require.NoError(t, err)
	_, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, &displayName)
	require.NoError(t, err)
}

func TestListPersonRelationshipsRendersBothDirectionsFromOneRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)
	mustSetPersonDisplayName(t, f, alice, "alice")

	// "alice is the parent of bob" — stored once.
	edge, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "parent",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	fromAlice, err := f.Store.ListPersonRelationshipsContext(ctx, alice,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(fromAlice, 1)
	assert.Equal(edge.ID, fromAlice[0].Relationship.ID)
	assert.Equal(store.RelationshipDirectionOutgoing, fromAlice[0].Direction)
	assert.Equal(bob, fromAlice[0].CounterpartPersonID)
	assert.Equal("child", fromAlice[0].CounterpartLabel, "bob is alice's child")

	fromBob, err := f.Store.ListPersonRelationshipsContext(ctx, bob,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(fromBob, 1)
	assert.Equal(edge.ID, fromBob[0].Relationship.ID, "the same single row serves both views")
	assert.Equal(store.RelationshipDirectionIncoming, fromBob[0].Direction)
	assert.Equal(alice, fromBob[0].CounterpartPersonID)
	assert.Equal("parent", fromBob[0].CounterpartLabel, "alice is bob's parent")

	// The counterpart identity comes along so a caller needs no second query.
	require.NotNil(fromBob[0].CounterpartDisplayName)
	assert.Equal("alice", *fromBob[0].CounterpartDisplayName)
	assert.NotEmpty(fromBob[0].CounterpartVCardUID)
}

func TestListPersonRelationshipsRendersSymmetricTypesIdenticallyFromBothEnds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)

	_, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "spouse",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	fromAlice, err := f.Store.ListPersonRelationshipsContext(ctx, alice,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(fromAlice, 1)
	fromBob, err := f.Store.ListPersonRelationshipsContext(ctx, bob,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(fromBob, 1)

	assert.Equal("spouse", fromAlice[0].CounterpartLabel)
	assert.Equal("spouse", fromBob[0].CounterpartLabel)
	assert.Equal(fromAlice[0].Relationship.ID, fromBob[0].Relationship.ID)
	assert.Equal(bob, fromAlice[0].CounterpartPersonID)
	assert.Equal(alice, fromBob[0].CounterpartPersonID)
}

func TestListPersonRelationshipsHidesEndedEdgesUnlessAsked(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)

	ending, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "co-worker",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	until, err := store.ParseRelationshipDate("2022-01")
	require.NoError(err)
	_, err = f.Store.EndPersonRelationshipContext(ctx, ending.ID, ending.Revision, until, "user")
	require.NoError(err)

	current, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "friend",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	active, err := f.Store.ListPersonRelationshipsContext(ctx, alice,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(active, 1)
	assert.Equal(current.ID, active[0].Relationship.ID)

	all, err := f.Store.ListPersonRelationshipsContext(ctx, alice,
		store.PersonRelationshipListOptions{IncludeEnded: true})
	require.NoError(err)
	require.Len(all, 2)
	// Active edges sort ahead of history.
	assert.Equal(store.RelationshipStatusActive, all[0].Relationship.Status)
	assert.Equal(store.RelationshipStatusEnded, all[1].Relationship.Status)
	require.NotNil(all[1].Relationship.EndDate)
	assert.Equal("2022-01", all[1].Relationship.EndDate.String())
}

func TestListPersonRelationshipsIsDeterministicAndScopedToOnePerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice := mustPerson(t, f, "alice@example.com", "alice")
	bob := mustPerson(t, f, "bob@example.com", "bob")
	carol := mustPerson(t, f, "carol@example.com", "carol")

	for _, target := range []int64{carol, bob} {
		_, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
			SourcePersonID: alice, TargetPersonID: target, TypeSlug: "friend",
			Source: store.ProvenanceUser, Actor: "user",
		})
		require.NoError(err)
	}
	_, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: bob, TargetPersonID: carol, TypeSlug: "neighbor",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	views, err := f.Store.ListPersonRelationshipsContext(ctx, alice,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(views, 2, "bob<->carol is not alice's relationship")
	// Ordered by counterpart display name, so the result is stable across runs.
	assert.Equal(bob, views[0].CounterpartPersonID)
	assert.Equal(carol, views[1].CounterpartPersonID)

	empty, err := f.Store.ListPersonRelationshipsContext(ctx, 999999,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	assert.Empty(empty, "an unknown person has no relationships, not an error")
}

func TestListPersonRelationshipsCountsEachEdgeOnceForOnePerson(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)

	// Mutual asymmetric edges are two distinct rows; each must appear once,
	// in the direction that person occupies.
	_, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "agent",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: bob, TargetPersonID: alice, TypeSlug: "agent",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	views, err := f.Store.ListPersonRelationshipsContext(ctx, alice,
		store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(views, 2)
	directions := []store.RelationshipDirection{views[0].Direction, views[1].Direction}
	assert.Contains(directions, store.RelationshipDirectionOutgoing)
	assert.Contains(directions, store.RelationshipDirectionIncoming)
	labels := []string{views[0].CounterpartLabel, views[1].CounterpartLabel}
	assert.Contains(labels, "principal", "alice is bob's agent, so bob is the principal")
	assert.Contains(labels, "agent", "bob is alice's agent")
}

func TestCounterpartLabelHelperMatchesTheQueryProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	alice, bob := mustTwoPersons(t, f)

	for _, slug := range []string{"parent", "agent", "emergency", "muse", "crush", "friend"} {
		relationshipType, err := f.Store.GetRelationshipTypeBySlugContext(ctx, slug)
		require.NoError(err)
		assert.Equalf(relationshipType.ReverseLabel,
			relationshipType.CounterpartLabel(store.RelationshipDirectionOutgoing),
			"type %q outgoing label", slug)
		assert.Equalf(relationshipType.ForwardLabel,
			relationshipType.CounterpartLabel(store.RelationshipDirectionIncoming),
			"type %q incoming label", slug)
	}

	edge, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: alice, TargetPersonID: bob, TypeSlug: "emergency",
		Source: store.ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	relationshipType, err := f.Store.GetRelationshipTypeContext(ctx, edge.RelationshipTypeID)
	require.NoError(err)

	for personID, direction := range map[int64]store.RelationshipDirection{
		alice: store.RelationshipDirectionOutgoing,
		bob:   store.RelationshipDirectionIncoming,
	} {
		views, err := f.Store.ListPersonRelationshipsContext(ctx, personID,
			store.PersonRelationshipListOptions{})
		require.NoError(err)
		require.Len(views, 1)
		assert.Equal(direction, views[0].Direction)
		assert.Equal(relationshipType.CounterpartLabel(direction), views[0].CounterpartLabel)
	}
}
