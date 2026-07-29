package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonPromoteGetListUpdateAndRevisionConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	alias := f.EnsureParticipant("alice+alias@example.com", "alice", "example.com")
	_, err := f.Store.LinkParticipants(alice, alias)
	require.NoError(err)

	created, err := f.Store.CreatePersonFromParticipant(alias)
	require.NoError(err)
	assert.Positive(created.ID)
	assert.Regexp(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, created.VCardUID)
	assert.Nil(created.DisplayName)
	assert.Equal(int64(1), created.Revision)
	assert.Equal([]int64{alice, alias}, created.ParticipantIDs)

	promotedAgain, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	assert.Equal(created.ID, promotedAgain.ID)
	assert.Equal(created.VCardUID, promotedAgain.VCardUID)

	got, err := f.Store.GetPerson(created.ID)
	require.NoError(err)
	assert.Equal(*created, *got)

	persons, err := f.Store.ListPersons()
	require.NoError(err)
	require.Len(persons, 1)
	assert.Equal(created.ID, persons[0].ID)
	assert.Equal([]int64{alice, alias}, persons[0].ParticipantIDs)

	displayName := "  alice  "
	updated, err := f.Store.UpdatePersonDisplayName(created.ID, created.Revision, &displayName)
	require.NoError(err)
	require.NotNil(updated.DisplayName)
	assert.Equal("alice", *updated.DisplayName)
	assert.Equal(created.Revision+1, updated.Revision)

	_, err = f.Store.UpdatePersonDisplayName(created.ID, created.Revision, &displayName)
	assert.ErrorIs(err, store.ErrPersonRevisionConflict)
}

func TestLinkParticipantsRejectsDifferentCuratedPersons(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	bob := f.EnsureParticipant("bob@example.com", "bob", "example.com")
	alicePerson, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	bobPerson, err := f.Store.CreatePersonFromParticipant(bob)
	require.NoError(err)

	_, err = f.Store.LinkParticipants(alice, bob)
	require.ErrorIs(err, store.ErrPersonBindingConflict)
	var conflict *store.PersonBindingConflictError
	require.ErrorAs(err, &conflict)
	assert.ElementsMatch([]int64{alicePerson.ID, bobPerson.ID}, conflict.PersonIDs)

	assert.Equal([]int64{alice}, mustClusterMembers(t, f.Store, alice))
	assert.Equal([]int64{bob}, mustClusterMembers(t, f.Store, bob))
}

func TestMergeParticipantsPreservesOrRejectsPersonBinding(t *testing.T) {
	t.Run("preserves loser binding on winner", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := storetest.New(t)
		winner := f.EnsureParticipant("alice@example.com", "alice", "example.com")
		loser := f.EnsureParticipant("alice+alias@example.com", "alice", "example.com")
		person, err := f.Store.CreatePersonFromParticipant(loser)
		require.NoError(err)

		require.NoError(f.Store.MergeParticipants(loser, winner))
		got, err := f.Store.GetPerson(person.ID)
		require.NoError(err)
		assert.Equal(person.VCardUID, got.VCardUID)
		assert.Equal([]int64{winner}, got.ParticipantIDs)
		assert.Equal(person.Revision+1, got.Revision)
	})

	t.Run("rejects different persons before merge", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := storetest.New(t)
		alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
		bob := f.EnsureParticipant("bob@example.com", "bob", "example.com")
		_, err := f.Store.CreatePersonFromParticipant(alice)
		require.NoError(err)
		_, err = f.Store.CreatePersonFromParticipant(bob)
		require.NoError(err)

		err = f.Store.MergeParticipants(bob, alice)
		require.ErrorIs(err, store.ErrPersonBindingConflict)
		assert.Equal(int64(1), participantCount(t, f.Store, bob))
	})
}

func TestPromoteIntoExistingPersonBumpsRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	alias := f.EnsureParticipant("alice+alias@example.com", "alice", "example.com")
	person, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	assert.Equal(int64(1), person.Revision)

	_, err = f.Store.LinkParticipants(alice, alias)
	require.NoError(err)
	expanded, err := f.Store.CreatePersonFromParticipant(alias)
	require.NoError(err)
	assert.Equal(person.ID, expanded.ID)
	assert.Equal(person.Revision+1, expanded.Revision)
	assert.Equal([]int64{alice, alias}, expanded.ParticipantIDs)

	displayName := "alice"
	_, err = f.Store.UpdatePersonDisplayName(person.ID, person.Revision, &displayName)
	assert.ErrorIs(err, store.ErrPersonRevisionConflict)
}

func TestPersonIdentitySurvivesLinkUnlinkChurn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	alias := f.EnsureParticipant("alice+alias@example.com", "alice", "example.com")
	bob := f.EnsureParticipant("bob@example.com", "bob", "example.com")
	_, err := f.Store.LinkParticipants(alice, alias)
	require.NoError(err)
	person, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)

	_, err = f.Store.UnlinkParticipants(alice, alias)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(alias, bob)
	require.NoError(err)
	_, err = f.Store.UnlinkParticipants(alias, bob)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(alice, alias)
	require.NoError(err)

	got, err := f.Store.GetPerson(person.ID)
	require.NoError(err)
	assert.Equal(person.ID, got.ID)
	assert.Equal(person.VCardUID, got.VCardUID)
	assert.Equal([]int64{alice, alias}, got.ParticipantIDs)
}

func TestConcurrentLinkAndMergeKeepPersonBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	const groups = 8

	for i := range groups {
		winner := f.EnsureParticipant(fmt.Sprintf("alice+%d@example.com", i), "alice", "example.com")
		loser := f.EnsureParticipant(fmt.Sprintf("alice+alias%d@example.com", i), "alice", "example.com")
		person, err := f.Store.CreatePersonFromParticipant(winner)
		require.NoError(err)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var linkErr, mergeErr error
		go func() {
			defer wg.Done()
			<-start
			_, linkErr = f.Store.LinkParticipants(winner, loser)
		}()
		go func() {
			defer wg.Done()
			<-start
			mergeErr = f.Store.MergeParticipants(loser, winner)
		}()
		close(start)
		wg.Wait()

		require.NoError(mergeErr)
		if linkErr != nil {
			assert.True(errors.Is(linkErr, store.ErrParticipantNotFound) ||
				errors.Is(linkErr, store.ErrAlreadyLinked), "unexpected link error: %v", linkErr)
		}
		got, err := f.Store.GetPerson(person.ID)
		require.NoError(err)
		assert.Equal([]int64{winner}, got.ParticipantIDs)
	}
}

func mustClusterMembers(t *testing.T, st *store.Store, id int64) []int64 {
	t.Helper()
	members, err := st.ClusterMembers(id)
	require.NoError(t, err)
	return members
}

func participantCount(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	var count int64
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM participants WHERE id = ?`), id,
	).Scan(&count))
	return count
}
