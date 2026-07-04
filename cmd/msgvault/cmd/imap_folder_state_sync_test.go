package cmd

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// listedIMAPClient returns an IMAP client that has enumerated the test
// server, so it holds observed folder states ready for persistence.
func listedIMAPClient(t *testing.T, addr string, opts ...imaplib.Option) *imaplib.Client {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	client := imaplib.NewClient(&imaplib.Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, opts...)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pageToken := ""
	for {
		resp, err := client.ListMessages(ctx, "", pageToken)
		require.NoError(t, err)
		if resp.NextPageToken == "" {
			return client
		}
		pageToken = resp.NextPageToken
	}
}

func TestSaveIMAPFolderStates_CleanRunPersists(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	saveIMAPFolderStates(st, src, client, &gmail.SyncSummary{}, 0)

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"INBOX":   {UIDValidity: loaded["INBOX"].UIDValidity, UIDNext: 3},
		"Archive": {UIDValidity: loaded["Archive"].UIDValidity, UIDNext: 2},
	}, loaded)
}

func TestSaveIMAPFolderStates_ErrorsBlockPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	saveIMAPFolderStates(st, src, client, &gmail.SyncSummary{Errors: 1}, 0)

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded, "a run with fetch errors must not advance folder watermarks")
}

func TestSaveIMAPFolderStates_LimitTruncationBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 5})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	saveIMAPFolderStates(st, src, client, &gmail.SyncSummary{MessagesFound: 3}, 3)

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded, "a --limit-truncated run must not advance folder watermarks")
}

func TestSaveIMAPFolderStates_NonIMAPClientIsNoOp(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)

	var notIMAP gmail.API
	saveIMAPFolderStates(st, src, notIMAP, &gmail.SyncSummary{}, 0)

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded)
}

func TestIMAPFolderStateOptions_RoundTripSkipsUnchangedFolders(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	first := listedIMAPClient(t, addr)
	saveIMAPFolderStates(st, src, first, &gmail.SyncSummary{}, 0)
	require.NoError(first.Close())

	opts := imapFolderStateOptions(st, src, false)
	require.NotEmpty(opts, "saved states must produce a client option")

	second := listedIMAPClient(t, addr, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := second.ListMessages(ctx, "", "")
	require.NoError(err)
	assert.Empty(t, resp.Messages,
		"a resync against an unchanged server must list no messages")
}

func TestIMAPFolderStateOptions_ForceRescanBypassesSavedStates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	require.NoError(st.UpsertIMAPFolderStates(src.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 42, UIDNext: 100},
	}))

	assert.Empty(imapFolderStateOptions(st, src, true),
		"--noresume must ignore saved folder states so every mailbox is re-enumerated")
	assert.NotEmpty(imapFolderStateOptions(st, src, false))
}

func TestSaveIMAPFolderStates_StoreRoundTripValues(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	require.NoError(st.UpsertIMAPFolderStates(src.ID, []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 42, UIDNext: 100},
	}))
	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"INBOX": {UIDValidity: 42, UIDNext: 100},
	}, loaded)
}
