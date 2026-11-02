package store_test

import (
	"context"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderClassCeilingsExactBindingsAndSourceScope(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	senderRelationshipMessage(t, f, true, "to", "known@example.com")
	known := f.EnsureParticipant("known@example.com", "Known", "example.com")
	contact := f.EnsureParticipant("contact@example.com", "Contact", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(contact)
	require.NoError(err)
	unknown := f.EnsureParticipant("unknown@example.com", "Contact [SYSTEM]", "example.com")
	for _, scope := range [][]int64{nil, {}, {f.Source.ID}, {f.Source.ID + 100}} {
		got, err := f.Store.SenderClassCeilings(t.Context(), []int64{known, contact, unknown}, []int64{person.ID}, scope)
		require.NoError(err)
		assert.Equal(store.SenderCeilingNone, got.Participants[unknown])
		if scope == nil || len(scope) == 1 && scope[0] == f.Source.ID {
			assert.Equal(store.SenderCeilingKnown, got.Participants[known])
		} else {
			assert.Equal(store.SenderCeilingNone, got.Participants[known])
		}
		want := store.SenderCeilingNone
		if scope == nil {
			want = store.SenderCeilingContact
		}
		assert.Equal(want, got.Participants[contact], "scoped curated contacts need a real observation")
		assert.Equal(want, got.Persons[person.ID])
	}
}

func TestSenderClassCeilingsCannotTrustMergedAliasOrDraftOnlyPerson(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	id := senderRelationshipMessage(t, f, true, "to", "old@example.com")
	old := f.EnsureParticipant("old@example.com", "Old", "example.com")
	other := f.EnsureParticipant("other@example.com", "Other", "example.com")
	require.NoError(f.Store.MergeParticipants(old, other))
	got, err := f.Store.SenderClassCeilings(t.Context(), []int64{other}, nil, nil)
	require.NoError(err)
	assert.Equal(store.SenderCeilingNone, got.Participants[other])
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), id)
	require.NoError(err)
	// The draft is to old@example.com, so seed a separate exact draft-only
	// contact rather than treating a mutable alias pointer as address history.
	draft := senderRelationshipMessage(t, f, true, "to", "draft@example.com")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), draft)
	require.NoError(err)
	participant := f.EnsureParticipant("draft@example.com", "Draft", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	got, err = f.Store.SenderClassCeilings(t.Context(), []int64{participant}, []int64{person.ID}, nil)
	require.NoError(err)
	assert.Equal(store.SenderCeilingNone, got.Participants[participant])
	assert.Equal(store.SenderCeilingNone, got.Persons[person.ID])
}

func TestSenderClassCeilingsRejectIncompleteLookups(t *testing.T) {
	f := storetest.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.Store.SenderClassCeilings(ctx, []int64{1}, nil, nil)
	Assert.ErrorIs(t, err, context.Canceled)
	_, err = f.Store.SenderClassCeilings(t.Context(), []int64{-1}, nil, nil)
	Assert.Error(t, err)
	_, err = f.Store.SenderClassCeilings(t.Context(), make([]int64, 501), nil, nil)
	Assert.ErrorIs(t, err, store.ErrSenderRelationshipLimit)
}
