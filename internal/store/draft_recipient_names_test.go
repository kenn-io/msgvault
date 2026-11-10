package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
)

func TestCuratedNamesForAddresses(t *testing.T) {
	f := storetest.New(t)
	id := f.EnsureParticipant("alex@example.com", "Alex Rivera", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), id)
	require.NoError(t, err)
	given, nick, formatted := "Alex", "Al", "Alex Rivera"
	_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameStructured, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, GivenName: &given})
	require.NoError(t, err)
	_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameNickname, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, Formatted: &nick})
	require.NoError(t, err)
	_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameFormatted, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, Formatted: &formatted})
	require.NoError(t, err)
	got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"ALEX@example.com", "pat@example.com"}, nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Alex", "Al"}, got["alex@example.com"])
	assert.NotContains(t, got, "pat@example.com")
	for _, scope := range [][]int64{{}, {f.Source.ID + 100}} {
		got, err = f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, scope)
		require.NoError(t, err)
		assert.Empty(t, got)
	}
	msgID := f.CreateMessage("history")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), id, msgID)
	require.NoError(t, err)
	got, err = f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, []int64{f.Source.ID})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Alex", "Al"}, got["alex@example.com"])
}
