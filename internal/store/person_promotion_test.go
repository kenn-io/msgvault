package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonPromotionSeedsObservedName(t *testing.T) {
	for _, tc := range []struct {
		name         string
		names        []string
		promoteIndex int
		want         *string
	}{
		{name: "named", names: []string{"Alex Example"}, want: new("Alex Example")},
		{name: "nameless", names: []string{""}},
		{name: "blank", names: []string{" \t\n "}},
		{name: "named alias", names: []string{"", "Alex Example"}, want: new("Alex Example")},
		{name: "first named member", names: []string{"Alex Example", "Other Example"}, promoteIndex: 1, want: new("Alex Example")},
		{name: "skip whitespace member", names: []string{"\t\n", "  Alex Example  "}, want: new("Alex Example")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			ids := make([]int64, len(tc.names))
			for i, name := range tc.names {
				ids[i] = f.EnsureParticipant(fmt.Sprintf("promotion-%d@example.com", i), name, "example.com")
				if i > 0 {
					_, err := f.Store.LinkParticipants(ids[0], ids[i])
					require.NoError(err)
				}
			}
			person, created, err := f.Store.CreatePersonFromParticipant(ids[tc.promoteIndex])
			require.NoError(err)
			require.True(created)
			assert.Equal(tc.want, person.DisplayName)
			assert.Equal(int64(1), person.Revision)
			assert.Equal(ids, person.ParticipantIDs)
		})
	}
}

func TestPersonRepromotionPreservesEditedOrClearedName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *string
	}{
		{name: "edited", value: new("Custom Example")},
		{name: "cleared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			id := f.EnsureParticipant("promotion@example.com", "Alex Example", "example.com")
			person, _, err := f.Store.CreatePersonFromParticipant(id)
			require.NoError(err)
			person, err = f.Store.UpdatePersonDisplayName(person.ID, person.Revision, tc.value)
			require.NoError(err)
			got, created, err := f.Store.CreatePersonFromParticipant(id)
			require.NoError(err)
			assert.False(created)
			assert.Equal(person, got)
			got, created, err = f.Store.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), id, new("Replacement"))
			require.NoError(err)
			assert.False(created)
			assert.Equal(person, got)
		})
	}
}

func TestPersonMergePreservesSeededSurvivorName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("survivor@example.com", "Survivor Example", "example.com")
	b := f.EnsureParticipant("absorbed@example.com", "Absorbed Example", "example.com")
	survivor, _, err := f.Store.CreatePersonFromParticipant(a)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(b)
	require.NoError(err)
	result, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "promotion-merge", Actor: "test",
	})
	require.NoError(err)
	assert.Equal(new("Survivor Example"), result.Person.DisplayName)
}
