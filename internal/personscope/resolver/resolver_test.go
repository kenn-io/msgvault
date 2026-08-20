package resolver_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

type resolverStore struct {
	persons  map[int64]*store.Person
	clusters map[int64][]int64
	bindings map[int64]*store.Person
}

type clusterOnlyStore struct{ members []int64 }

func (s clusterOnlyStore) ClusterMembers(int64) ([]int64, error) {
	return slices.Clone(s.members), nil
}

func (s resolverStore) GetPersonContext(_ context.Context, id int64) (*store.Person, error) {
	person := s.persons[id]
	if person == nil {
		return nil, store.ErrPersonNotFound
	}
	clone := *person
	clone.ParticipantIDs = slices.Clone(person.ParticipantIDs)
	return &clone, nil
}

func (s resolverStore) ClusterMembers(id int64) ([]int64, error) {
	members := s.clusters[id]
	if len(members) == 0 {
		return []int64{id}, nil
	}
	return slices.Clone(members), nil
}

func (s resolverStore) PersonForParticipantsContext(
	_ context.Context,
	participantIDs []int64,
) (*store.Person, error) {
	for _, participantID := range participantIDs {
		if person := s.bindings[participantID]; person != nil {
			clone := *person
			clone.ParticipantIDs = slices.Clone(person.ParticipantIDs)
			return &clone, nil
		}
	}
	return nil, nil //nolint:nilnil // No matching durable binding is a valid lookup result.
}

func TestResolveDurablePersonUsesEveryBinding(t *testing.T) {
	assert := assert.New(t)
	profiles := resolverStore{persons: map[int64]*store.Person{
		40: {ID: 40, ParticipantIDs: []int64{2, 7}},
	}}

	resolved, err := resolver.Resolve(t.Context(), profiles,
		resolver.Reference{Kind: resolver.ReferencePerson, ID: 40}, nil)

	require.NoError(t, err)
	assert.Equal(int64(40), resolved.PersonID)
	assert.Equal([]int64{2, 7}, resolved.Scope.ParticipantIDs)
	assert.Equal([]personscope.Direction{
		personscope.FromPerson, personscope.ToPerson, personscope.Group,
	}, resolved.Scope.Directions)
	assert.True(resolved.Scope.IncludeUnclassifiedRosterRows)
}

func TestResolveDurablePersonWithoutBindingsReportsEmptyPopulation(t *testing.T) {
	profiles := resolverStore{persons: map[int64]*store.Person{40: {ID: 40}}}

	_, err := resolver.Resolve(t.Context(), profiles,
		resolver.Reference{Kind: resolver.ReferencePerson, ID: 40}, nil)

	require.ErrorIs(t, err, resolver.ErrEmptyPopulation)
}

func TestResolveParticipantTranslatesThroughDurablePerson(t *testing.T) {
	assert := assert.New(t)
	person := &store.Person{ID: 40, ParticipantIDs: []int64{2, 7}}
	profiles := resolverStore{
		clusters: map[int64][]int64{2: {2, 3}},
		bindings: map[int64]*store.Person{2: person},
	}

	resolved, err := resolver.Resolve(t.Context(), profiles,
		resolver.Reference{Kind: resolver.ReferenceParticipant, ID: 2},
		[]personscope.Direction{personscope.Group, personscope.FromPerson, personscope.FromPerson})

	require.NoError(t, err)
	assert.Equal(int64(40), resolved.PersonID)
	assert.Equal([]int64{2, 7}, resolved.Scope.ParticipantIDs,
		"durable bindings remain the population even after the observed cluster splits")
	assert.Equal([]personscope.Direction{personscope.FromPerson, personscope.Group},
		resolved.Scope.Directions)
	assert.False(resolved.Scope.IncludeUnclassifiedRosterRows)
}

func TestResolveParticipantKeepsClusterWhenDurableBindingHasNoParticipants(t *testing.T) {
	profiles := resolverStore{
		clusters: map[int64][]int64{2: {2, 3}},
		bindings: map[int64]*store.Person{2: {ID: 40}},
	}

	resolved, err := resolver.Resolve(t.Context(), profiles,
		resolver.Reference{Kind: resolver.ReferenceParticipant, ID: 2}, nil)

	require.NoError(t, err)
	assert.Equal(t, int64(40), resolved.PersonID)
	assert.Equal(t, []int64{2, 3}, resolved.Scope.ParticipantIDs)
}

func TestResolveUnpromotedParticipantUsesIdentityCluster(t *testing.T) {
	assert := assert.New(t)
	profiles := resolverStore{clusters: map[int64][]int64{9: {9, 10}}}

	resolved, err := resolver.Resolve(t.Context(), profiles,
		resolver.Reference{Kind: resolver.ReferenceParticipant, ID: 9},
		[]personscope.Direction{personscope.ToPerson})

	require.NoError(t, err)
	assert.Zero(resolved.PersonID)
	assert.Equal([]int64{9, 10}, resolved.Scope.ParticipantIDs)
	assert.Equal([]personscope.Direction{personscope.ToPerson}, resolved.Scope.Directions)
}

func TestResolveParticipantWorksWithoutDurableProfileCapability(t *testing.T) {
	resolved, err := resolver.Resolve(t.Context(), clusterOnlyStore{members: []int64{5, 6}},
		resolver.Reference{Kind: resolver.ReferenceParticipant, ID: 5}, nil)

	require.NoError(t, err)
	assert.Equal(t, []int64{5, 6}, resolved.Scope.ParticipantIDs)
}

func TestResolveRejectsUnknownPersonAndDirection(t *testing.T) {
	require := require.New(t)
	profiles := resolverStore{}

	_, err := resolver.Resolve(t.Context(), profiles,
		resolver.Reference{Kind: resolver.ReferencePerson, ID: 99}, nil)
	require.Error(err)
	require.ErrorIs(err, store.ErrPersonNotFound)

	_, err = resolver.Resolve(t.Context(), profiles,
		resolver.Reference{Kind: resolver.ReferenceParticipant, ID: 2},
		[]personscope.Direction{"sideways"})
	require.Error(err)
	assert.ErrorContains(t, err, "unknown person file direction")
}
