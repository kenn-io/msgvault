package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIMAPFolderStates_EmptyForNewSource(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-empty@example.com")
	require.NoError(err)

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.Empty(t, states)
}

func TestIMAPFolderStates_RoundTrip(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-roundtrip@example.com")
	require.NoError(err)

	saved := []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1234, UIDNext: 501, HighestModSeq: 9001},
		{Mailbox: "Archive/2009", UIDValidity: 99, UIDNext: 100000, HighestModSeq: 42},
	}
	require.NoError(st.UpsertIMAPFolderStates(source.ID, saved))

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.ElementsMatch(t, saved, states)
}

func TestIMAPFolderStates_UpsertOverwrites(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-overwrite@example.com")
	require.NoError(err)

	require.NoError(st.UpsertIMAPFolderStates(source.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 10, HighestModSeq: 100},
		{Mailbox: "Sent", UIDValidity: 2, UIDNext: 20, HighestModSeq: 200},
	}))
	// INBOX advances; Sent is untouched by this save.
	require.NoError(st.UpsertIMAPFolderStates(source.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 15, HighestModSeq: 150},
	}))

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.ElementsMatch(t, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 15, HighestModSeq: 150},
		{Mailbox: "Sent", UIDValidity: 2, UIDNext: 20, HighestModSeq: 200},
	}, states)
}

func TestIMAPFolderStates_IsolatedPerSource(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	a, err := st.GetOrCreateSource("imap", "folders-a@example.com")
	require.NoError(err)
	b, err := st.GetOrCreateSource("imap", "folders-b@example.com")
	require.NoError(err)

	require.NoError(st.UpsertIMAPFolderStates(a.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 7, UIDNext: 70},
	}))

	states, err := st.GetIMAPFolderStates(b.ID)
	require.NoError(err)
	assert.Empty(t, states)
}

func TestIMAPFolderStates_MaxUint32Values(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "folders-max@example.com")
	require.NoError(err)

	saved := []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 4294967295, UIDNext: 4294967295, HighestModSeq: ^uint64(0)},
	}
	require.NoError(st.UpsertIMAPFolderStates(source.ID, saved))

	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.Equal(t, saved, states)
}
