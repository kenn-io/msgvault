package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonDisplayNameRevision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	counter := func(want int64) {
		t.Helper()
		got, err := f.Store.PersonDisplayNameRevision()
		requirements.NoError(err)
		assertions.Equal(want, got)
	}
	counter(0)
	alice := f.EnsureParticipant("alice@example.com", "Alice Observed", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(alice)
	requirements.NoError(err)
	counter(0)
	person, err = f.Store.UpdatePersonDisplayName(person.ID, person.Revision, nil)
	requirements.NoError(err)
	counter(0)
	identityBefore, err := f.Store.IdentityRevision()
	requirements.NoError(err)
	name := "Alice Curated"
	updated, err := f.Store.UpdatePersonDisplayName(person.ID, person.Revision, &name)
	requirements.NoError(err)
	counter(1)
	identityAfter, err := f.Store.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(identityBefore, identityAfter)
	_, err = f.Store.UpdatePersonDisplayName(person.ID, person.Revision, &name)
	requirements.ErrorIs(err, store.ErrPersonRevisionConflict)
	counter(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.Store.UpdatePersonDisplayNameContext(ctx, updated.ID, updated.Revision, &name)
	requirements.Error(err)
	counter(1)
	for _, sameName := range []string{name, "  " + name + "  "} {
		updated, err = f.Store.UpdatePersonDisplayName(updated.ID, updated.Revision, &sameName)
		requirements.NoError(err)
		counter(1)
	}
	updated, err = f.Store.UpdatePersonDisplayName(updated.ID, updated.Revision, nil)
	requirements.NoError(err)
	counter(2)
	blank := "  "
	updated, err = f.Store.UpdatePersonDisplayName(updated.ID, updated.Revision, &blank)
	requirements.NoError(err)
	counter(2)
	profile, err := f.Store.ApplyPersonProfilePatchContext(context.Background(), updated.ID, updated.Revision, store.PersonProfilePatch{
		Categories: &store.PersonCategoryPatch{Add: []store.PersonCategoryInput{{OriginalValue: "Test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}}}},
	})
	requirements.NoError(err)
	counter(2)
	requirements.NoError(f.Store.DeletePerson(updated.ID, profile.Person.Revision))
	counter(2)
	t.Log("counter: create=0 rename=1 CAS miss=1 cancellation=1 repeated rename=1 clear=2 repeated clear=2 profile patch=2 delete=2")
}
