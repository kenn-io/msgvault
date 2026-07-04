package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIMAPFolderStates_EmptyForNewSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-empty@example.com")
	require.NoError(t, err)

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(t, err)
	assert.Empty(t, states)
}

func TestIMAPFolderStates_RoundTrip(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-roundtrip@example.com")
	require.NoError(t, err)

	saved := []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1234, UIDNext: 501},
		{Mailbox: "Archive/2009", UIDValidity: 99, UIDNext: 100000},
	}
	require.NoError(t, st.UpsertIMAPFolderStates(source.ID, saved))

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, saved, states)
}

func TestIMAPFolderStates_UpsertOverwrites(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-overwrite@example.com")
	require.NoError(t, err)

	require.NoError(t, st.UpsertIMAPFolderStates(source.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 10},
		{Mailbox: "Sent", UIDValidity: 2, UIDNext: 20},
	}))
	// INBOX advances; Sent is untouched by this save.
	require.NoError(t, st.UpsertIMAPFolderStates(source.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 15},
	}))

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 15},
		{Mailbox: "Sent", UIDValidity: 2, UIDNext: 20},
	}, states)
}

func TestIMAPFolderStates_IsolatedPerSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	a, err := st.GetOrCreateSource("imap", "folders-a@example.com")
	require.NoError(t, err)
	b, err := st.GetOrCreateSource("imap", "folders-b@example.com")
	require.NoError(t, err)

	require.NoError(t, st.UpsertIMAPFolderStates(a.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 7, UIDNext: 70},
	}))

	states, err := st.GetIMAPFolderStates(b.ID)
	require.NoError(t, err)
	assert.Empty(t, states)
}

func TestIMAPFolderStates_MaxUint32Values(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-max@example.com")
	require.NoError(t, err)

	saved := []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 4294967295, UIDNext: 4294967295},
	}
	require.NoError(t, st.UpsertIMAPFolderStates(source.ID, saved))

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(t, err)
	assert.Equal(t, saved, states)
}
