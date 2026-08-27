package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
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
	revisionBeforePromotion, err := f.Store.IdentityRevision()
	require.NoError(err)

	created, wasCreated, err := f.Store.CreatePersonFromParticipant(alias)
	require.NoError(err)
	assert.True(wasCreated)
	revisionAfterPromotion, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revisionBeforePromotion+1, revisionAfterPromotion)
	assert.Positive(created.ID)
	assert.Regexp(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, created.VCardUID)
	assert.Nil(created.DisplayName)
	assert.Equal(int64(1), created.Revision)
	assert.Equal([]int64{alice, alias}, created.ParticipantIDs)

	promotedAgain, wasCreated, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	assert.False(wasCreated)
	revisionAfterRepromotion, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revisionAfterPromotion, revisionAfterRepromotion)
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
	assert.Equal(created.ParticipantIDs, updated.ParticipantIDs)

	_, err = f.Store.UpdatePersonDisplayName(created.ID, created.Revision, &displayName)
	assert.ErrorIs(err, store.ErrPersonRevisionConflict)
}

func TestLinkParticipantsRejectsDifferentCuratedPersons(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	bob := f.EnsureParticipant("bob@example.com", "bob", "example.com")
	alicePerson, _, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	bobPerson, _, err := f.Store.CreatePersonFromParticipant(bob)
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
		person, _, err := f.Store.CreatePersonFromParticipant(loser)
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
		_, _, err := f.Store.CreatePersonFromParticipant(alice)
		require.NoError(err)
		_, _, err = f.Store.CreatePersonFromParticipant(bob)
		require.NoError(err)

		err = f.Store.MergeParticipants(bob, alice)
		require.ErrorIs(err, store.ErrPersonBindingConflict)
		assert.Equal(int64(1), participantCount(t, f.Store, bob))
	})
}

func TestLinkAutoBindsNewClusterMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	alias := f.EnsureParticipant("alice+alias@example.com", "alice", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	assert.Equal(int64(1), person.Revision)

	// Linking an unbound participant into the promoted cluster extends the
	// person immediately; no re-promotion is needed.
	_, err = f.Store.LinkParticipants(alice, alias)
	require.NoError(err)
	expanded, err := f.Store.GetPerson(person.ID)
	require.NoError(err)
	assert.Equal(person.Revision+1, expanded.Revision)
	assert.Equal([]int64{alice, alias}, expanded.ParticipantIDs)

	promoted, wasCreated, err := f.Store.CreatePersonFromParticipant(alias)
	require.NoError(err)
	assert.False(wasCreated)
	assert.Equal(person.ID, promoted.ID)
	assert.Equal(expanded.Revision, promoted.Revision)

	displayName := "alice"
	_, err = f.Store.UpdatePersonDisplayName(person.ID, person.Revision, &displayName)
	assert.ErrorIs(err, store.ErrPersonRevisionConflict)
}

func TestLinkWithoutPersonsBindsNothing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	alias := f.EnsureParticipant("alice+alias@example.com", "alice", "example.com")
	_, err := f.Store.LinkParticipants(alice, alias)
	require.NoError(err)

	person, err := f.Store.PersonForParticipants([]int64{alice, alias})
	require.NoError(err)
	assert.Nil(person)
	persons, err := f.Store.ListPersons()
	require.NoError(err)
	assert.Empty(persons)
}

func TestRepromotionThatFillsBindingBumpsIdentityRevisionOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant(
		"promotion-fill@example.com", "Promotion Fill", "example.com")
	aliasID := f.EnsureParticipant(
		"promotion-fill-alias@example.com", "Promotion Fill Alias", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	require.True(created)

	// Simulate a pre-chokepoint archive where the cluster edge exists but its
	// newly connected member was never carried into person_participants.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO participant_links (participant_a, participant_b) VALUES (?, ?)
	`), participantID, aliasID)
	require.NoError(err)
	revisionBefore, err := f.Store.IdentityRevision()
	require.NoError(err)

	promoted, wasCreated, err := f.Store.CreatePersonFromParticipant(aliasID)
	require.NoError(err)
	assert.False(wasCreated)
	assert.Equal(person.ID, promoted.ID)
	assert.Equal([]int64{participantID, aliasID}, promoted.ParticipantIDs)
	revisionAfter, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter)
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
	person, _, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)

	_, err = f.Store.UnlinkParticipants(alice, alias)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(alias, bob)
	require.NoError(err)
	_, err = f.Store.UnlinkParticipants(alias, bob)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(alice, alias)
	require.NoError(err)

	// The person ID and vCard UID survive the churn. Linking bob auto-bound
	// him into the person, and unlink deliberately never unbinds, so bob
	// stays a member after the link is retracted (delete + re-promote is the
	// remedy for an over-wide person).
	got, err := f.Store.GetPerson(person.ID)
	require.NoError(err)
	assert.Equal(person.ID, got.ID)
	assert.Equal(person.VCardUID, got.VCardUID)
	assert.Equal([]int64{alice, alias, bob}, got.ParticipantIDs)
}

func TestConcurrentLinkAndMergeKeepPersonBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	const groups = 8

	for i := range groups {
		winner := f.EnsureParticipant(fmt.Sprintf("alice+%d@example.com", i), "alice", "example.com")
		loser := f.EnsureParticipant(fmt.Sprintf("alice+alias%d@example.com", i), "alice", "example.com")
		person, _, err := f.Store.CreatePersonFromParticipant(winner)
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

func TestDeletePersonRetiresProfileAndUnblocksLinking(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	bob := f.EnsureParticipant("bob@example.com", "bob", "example.com")
	alicePerson, _, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	bobPerson, _, err := f.Store.CreatePersonFromParticipant(bob)
	require.NoError(err)
	revisionBeforeFailedDeletes, err := f.Store.IdentityRevision()
	require.NoError(err)

	err = f.Store.DeletePerson(bobPerson.ID, bobPerson.Revision+1)
	require.ErrorIs(err, store.ErrPersonRevisionConflict)
	err = f.Store.DeletePerson(bobPerson.ID+1000, 1)
	require.ErrorIs(err, store.ErrPersonNotFound)
	revisionAfterFailedDeletes, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revisionBeforeFailedDeletes, revisionAfterFailedDeletes)

	require.NoError(f.Store.DeletePerson(bobPerson.ID, bobPerson.Revision))
	revisionAfterDelete, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revisionBeforeFailedDeletes+1, revisionAfterDelete)
	_, err = f.Store.GetPerson(bobPerson.ID)
	require.ErrorIs(err, store.ErrPersonNotFound)

	// Deleting bob's profile resolves the binding conflict: the clusters can
	// now be linked, and bob is auto-bound into alice's person.
	_, err = f.Store.LinkParticipants(alice, bob)
	require.NoError(err)
	got, err := f.Store.GetPerson(alicePerson.ID)
	require.NoError(err)
	assert.Equal([]int64{alice, bob}, got.ParticipantIDs)

	// The deleted person's UID is retired: re-promotion mints a new identity.
	require.NoError(f.Store.DeletePerson(alicePerson.ID, got.Revision))
	reborn, wasCreated, err := f.Store.CreatePersonFromParticipant(bob)
	require.NoError(err)
	assert.True(wasCreated)
	assert.NotEqual(alicePerson.ID, reborn.ID)
	assert.NotEqual(alicePerson.VCardUID, reborn.VCardUID)
	assert.Equal([]int64{alice, bob}, reborn.ParticipantIDs)
}

func TestMergeFillsPersonAcrossCombinedCluster(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	absorbed := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	winner := f.EnsureParticipant("alice+alias@example.com", "alice", "example.com")
	sibling := f.EnsureParticipant("alice+work@example.com", "alice", "example.com")
	_, err := f.Store.LinkParticipants(winner, sibling)
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(absorbed)
	require.NoError(err)

	require.NoError(f.Store.MergeParticipants(absorbed, winner))
	got, err := f.Store.GetPerson(person.ID)
	require.NoError(err)
	assert.Equal(person.VCardUID, got.VCardUID)
	assert.Equal([]int64{winner, sibling}, got.ParticipantIDs)
	assert.Greater(got.Revision, person.Revision)
}

func TestPersonMergeInvalidatesSweep(t *testing.T) {
	t.Run("participant merge", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := storetest.New(t)
		absorbed := f.EnsureParticipant(
			"merge-absorbed@example.test", "Absorbed", "example.test")
		survivor := f.EnsureParticipant(
			"merge-survivor@example.test", "Survivor", "example.test")
		messageID := f.CreateMessage("person-sweep-participant-merge")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`UPDATE messages SET sender_id = ? WHERE id = ?`), absorbed, messageID)
		requirements.NoError(err)
		person, _, err := f.Store.CreatePersonFromParticipant(absorbed)
		requirements.NoError(err)
		_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
		requirements.NoError(err)
		deletePersonSweepWork(t, f.Store, person.ID)
		before := latestPersonSweepSequence(t, f.Store)

		requirements.NoError(f.Store.MergeParticipants(absorbed, survivor))
		changes := personSweepChangesAfter(t, f.Store, person.ID, before)
		requirements.NotEmpty(changes)
		checks.Contains(archiveEffects(changes),
			peoplesweep.EvidenceEffectIdentityReassigned)
		rows, dirtyThrough := personSweepWorkState(t, f.Store, person.ID)
		checks.Equal(1, rows)
		checks.Equal(changes[len(changes)-1].Sequence, dirtyThrough)
	})

	t.Run("identity split", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := storetest.New(t)
		alice := f.EnsureParticipant("split-alice@example.test", "Alice", "example.test")
		alias := f.EnsureParticipant("split-alias@example.test", "Alias", "example.test")
		_, err := f.Store.LinkParticipants(alice, alias)
		requirements.NoError(err)
		person, _, err := f.Store.CreatePersonFromParticipant(alice)
		requirements.NoError(err)
		messageID := f.CreateMessage("person-sweep-identity-split")
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`UPDATE messages SET sender_id = ?, sent_at = ? WHERE id = ?`), alias,
			time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC), messageID)
		requirements.NoError(err)
		_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
		requirements.NoError(err)
		deletePersonSweepWork(t, f.Store, person.ID)
		before := latestPersonSweepSequence(t, f.Store)

		_, err = f.Store.UnlinkParticipants(alice, alias)
		requirements.NoError(err)
		changes := personSweepChangesAfter(t, f.Store, person.ID, before)
		requirements.NotEmpty(changes)
		checks.Contains(archiveEffects(changes),
			peoplesweep.EvidenceEffectIdentityReassigned)
		checks.Equal(messageID, changes[len(changes)-1].MessageID)
		rows, dirtyThrough := personSweepWorkState(t, f.Store, person.ID)
		checks.Equal(1, rows)
		checks.Equal(changes[len(changes)-1].Sequence, dirtyThrough)
	})
}

func TestUntrackedPersonIdentityMutationDoesNotPublishSweep(t *testing.T) {
	t.Run("participant merge", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := storetest.New(t)
		absorbed := f.EnsureParticipant(
			"untracked-merge-absorbed@example.test", "Absorbed", "example.test")
		survivor := f.EnsureParticipant(
			"untracked-merge-survivor@example.test", "Survivor", "example.test")
		messageID := f.CreateMessage("untracked-person-sweep-participant-merge")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`UPDATE messages SET sender_id = ? WHERE id = ?`), absorbed, messageID)
		requirements.NoError(err)
		person, _, err := f.Store.CreatePersonFromParticipant(absorbed)
		requirements.NoError(err)
		before := latestPersonSweepSequence(t, f.Store)

		requirements.NoError(f.Store.MergeParticipants(absorbed, survivor))
		checks.Equal(before, latestPersonSweepSequence(t, f.Store))
		checks.Empty(personSweepChangesAfter(t, f.Store, person.ID, 0))
	})

	t.Run("identity link", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := storetest.New(t)
		alice := f.EnsureParticipant(
			"untracked-link-alice@example.test", "Alice", "example.test")
		alias := f.EnsureParticipant(
			"untracked-link-alias@example.test", "Alias", "example.test")
		person, _, err := f.Store.CreatePersonFromParticipant(alice)
		requirements.NoError(err)
		messageID := f.CreateMessage("untracked-person-sweep-identity-link")
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`UPDATE messages SET sender_id = ? WHERE id = ?`), alias, messageID)
		requirements.NoError(err)
		before := latestPersonSweepSequence(t, f.Store)

		_, err = f.Store.LinkParticipants(alice, alias)
		requirements.NoError(err)
		checks.Equal(before, latestPersonSweepSequence(t, f.Store))
		checks.Empty(personSweepChangesAfter(t, f.Store, person.ID, 0))
	})
}

func TestPersonForParticipants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "alice", "example.com")
	bob := f.EnsureParticipant("bob@example.com", "bob", "example.com")

	person, err := f.Store.PersonForParticipants([]int64{alice, bob})
	require.NoError(err)
	assert.Nil(person)

	alicePerson, _, err := f.Store.CreatePersonFromParticipant(alice)
	require.NoError(err)
	person, err = f.Store.PersonForParticipants([]int64{alice})
	require.NoError(err)
	require.NotNil(person)
	assert.Equal(alicePerson.ID, person.ID)

	_, _, err = f.Store.CreatePersonFromParticipant(bob)
	require.NoError(err)
	_, err = f.Store.PersonForParticipants([]int64{alice, bob})
	assert.ErrorIs(err, store.ErrPersonBindingConflict)
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
