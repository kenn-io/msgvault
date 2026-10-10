package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type scopedPersonMergeStore interface {
	MergePersonsAuthorizedContext(ctx context.Context, request store.PersonMergeRequest, authorize store.PersonEditAuthorizer) (*store.PersonMergeResult, error)
}

// The callback must run before mutation and again before a receipt is replayed.
func TestPersonMergeAuthorizationDenialAndFreshReplay(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	backend, available := any(st).(scopedPersonMergeStore)
	requirements.True(available, "native scoped merge admission must exist")
	first := newTestPerson(t, st)
	participant, err := st.EnsureParticipant("absorbed-scope@example.test", "Absorbed Scope", "example.test")
	requirements.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	survivor, err := st.GetPerson(first)
	requirements.NoError(err)
	request := store.PersonMergeRequest{SurvivorID: first, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-scoped-merge", Actor: "agent:synthetic-merge-grant"}
	denied := errors.New("synthetic merge scope denied")
	attempts := 0
	authorize := func(_ context.Context, selection *store.IdentityGrantSelection) error {
		attempts++
		identities := map[int64]string{}
		for _, person := range selection.Persons {
			identities[person.ID] = person.UID
		}
		assertions.Equal(map[int64]string{first: survivor.VCardUID, absorbed.ID: absorbed.VCardUID}, identities)
		return denied
	}
	_, err = backend.MergePersonsAuthorizedContext(t.Context(), request, authorize)
	requirements.ErrorIs(err, denied)
	assertions.Equal(1, attempts)
	actualSurvivor, err := st.GetPerson(first)
	requirements.NoError(err)
	actualAbsorbed, err := st.GetPerson(absorbed.ID)
	requirements.NoError(err)
	assertions.Equal(survivor, actualSurvivor)
	assertions.Equal(absorbed, actualAbsorbed)
	committed, err := backend.MergePersonsAuthorizedContext(t.Context(), request, func(context.Context, *store.IdentityGrantSelection) error { return nil })
	requirements.NoError(err)
	_, err = st.GetPerson(absorbed.ID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
	replayScopeSeen := false
	replayAuthorize := func(_ context.Context, selection *store.IdentityGrantSelection) error {
		replayScopeSeen = true
		identities := map[int64]string{}
		for _, person := range selection.Persons {
			identities[person.ID] = person.UID
		}
		assertions.Equal(map[int64]string{first: survivor.VCardUID, absorbed.ID: absorbed.VCardUID}, identities)
		return denied
	}
	replay, err := backend.MergePersonsAuthorizedContext(t.Context(), request, replayAuthorize)
	requirements.ErrorIs(err, denied)
	assertions.Nil(replay)
	assertions.True(replayScopeSeen)
	replay, err = backend.MergePersonsAuthorizedContext(t.Context(), request, func(context.Context, *store.IdentityGrantSelection) error { return nil })
	requirements.NoError(err)
	wantReceipt, err := json.Marshal(committed)
	requirements.NoError(err)
	gotReceipt, err := json.Marshal(replay)
	requirements.NoError(err)
	assertions.JSONEq(string(wantReceipt), string(gotReceipt))
	actualSurvivor, err = st.GetPerson(first)
	requirements.NoError(err)
	assertions.Equal(committed.Person, *actualSurvivor)
}
