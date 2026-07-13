package imap

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func newTestClient(t *testing.T, addr string, opts ...Option) *Client {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	client := NewClient(&Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, opts...)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// listAllMessages drains every page of ListMessages.
func listAllMessages(t *testing.T, client *Client) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var ids []string
	pageToken := ""
	for {
		resp, err := client.ListMessages(ctx, "", pageToken)
		require.NoError(t, err)
		for _, msg := range resp.Messages {
			ids = append(ids, msg.ID)
		}
		if resp.NextPageToken == "" {
			return ids
		}
		pageToken = resp.NextPageToken
	}
}

func TestListMessages_RecordsFolderStates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})
	client := newTestClient(t, addr)

	ids := listAllMessages(t, client)
	assert.Len(ids, 5)

	states := client.ObservedFolderStates()
	require.Contains(states, "INBOX")
	require.Contains(states, "Archive")
	assert.Equal(uint32(3), states["INBOX"].UIDNext)
	assert.Equal(uint32(4), states["Archive"].UIDNext)
	assert.NotZero(states["INBOX"].UIDValidity)
}

func TestListMessages_SkipsUnchangedFolders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 5)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	second := newTestClient(t, addr, WithFolderStates(saved))
	ids := listAllMessages(t, second)
	assert.Empty(ids, "unchanged folders must not be re-enumerated")
	assert.Equal(saved, second.ObservedFolderStates(),
		"unchanged folders keep their saved state for the next save")
}

func TestListMessages_ListsOnlyNewMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 5)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	testutil.AppendIMAPMessage(t, user, "INBOX")

	second := newTestClient(t, addr, WithFolderStates(saved))
	ids := listAllMessages(t, second)
	assert.Equal([]string{"INBOX|3"}, ids,
		"only the message appended after the saved state should be listed")

	states := second.ObservedFolderStates()
	assert.Equal(uint32(4), states["INBOX"].UIDNext)
	assert.Equal(saved["Archive"], states["Archive"])
}

type listProgressCall struct {
	done, total      int
	mailbox          string
	found, unchanged int
}

type folderStateSave struct {
	mailbox string
	state   FolderState
}

func TestListMessages_ReportsListProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})

	var calls []listProgressCall
	record := func(done, total int, mailbox string, found, unchanged int) {
		calls = append(calls, listProgressCall{done, total, mailbox, found, unchanged})
	}

	first := newTestClient(t, addr, WithListProgress(record))
	require.Len(listAllMessages(t, first), 5)

	require.Len(calls, 3, "one initial call plus one per mailbox")
	assert.Equal(listProgressCall{done: 0, total: 2}, calls[0])
	final := calls[2]
	assert.Equal(2, final.done)
	assert.Equal(2, final.total)
	assert.Equal(5, final.found)
	assert.Equal(0, final.unchanged)

	// A resync with saved states reports every folder as unchanged.
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())
	calls = nil
	second := newTestClient(t, addr, WithListProgress(record), WithFolderStates(saved))
	require.Empty(listAllMessages(t, second))
	final = calls[len(calls)-1]
	assert.Equal(0, final.found)
	assert.Equal(2, final.unchanged)
}

func TestListMessages_UIDValidityChangeForcesFullRescan(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 2)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	// Simulate the server invalidating its UID space.
	stale := map[string]FolderState{
		"INBOX": {UIDValidity: saved["INBOX"].UIDValidity + 1, UIDNext: saved["INBOX"].UIDNext},
	}

	second := newTestClient(t, addr, WithFolderStates(stale))
	ids := listAllMessages(t, second)
	assert.Len(t, ids, 2, "UIDVALIDITY mismatch must trigger full enumeration")
}

func TestListMessages_DateFilterDisablesFolderTracking(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 2)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	since := time.Now().Add(-24 * time.Hour)
	second := newTestClient(t, addr,
		WithFolderStates(saved),
		WithDateFilter(since, time.Time{}))
	ids := listAllMessages(t, second)
	assert.Len(ids, 2, "date-filtered runs must ignore saved folder states")
	assert.Nil(second.ObservedFolderStates(),
		"date-filtered runs must not record folder states")
}

func TestListMessages_AllMailboxRecordsFolderStatesForNoopResync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"All Mail": 2, "Projects": 1})

	first := newTestClient(t, addr)
	// Seed mailbox discovery as if LIST reported All Mail with \All;
	// the in-memory server does not retain CreateOptions.SpecialUse.
	first.mailboxCache = []string{"All Mail", "Projects"}
	first.allMailFolder = "All Mail"
	require.Len(listAllMessages(t, first), 3)
	saved := first.ObservedFolderStates()
	require.Contains(saved, "All Mail")
	require.Contains(saved, "Projects")
	require.NoError(first.Close())

	second := newTestClient(t, addr, WithFolderStates(saved))
	second.mailboxCache = []string{"All Mail", "Projects"}
	second.allMailFolder = "All Mail"
	ids := listAllMessages(t, second)
	assert.Empty(ids, "unchanged folders must not be re-enumerated when an All Mail folder exists")
	assert.Equal(saved, second.ObservedFolderStates())
	assert.Nil(second.msgIDToLabels, "a no-op resync must not build the All Mail label map")
}

func TestAcknowledgeMessagesFlushesFolderStateWhenFolderComplete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 1})

	var saved []folderStateSave
	client := newTestClient(t, addr, WithFolderStateSave(func(mailbox string, state FolderState) {
		saved = append(saved, folderStateSave{mailbox: mailbox, state: state})
	}))
	require.Len(listAllMessages(t, client), 3)

	client.AcknowledgeMessages(context.Background(), []string{"INBOX|1"})
	assert.Empty(saved, "folder state must not be saved until every listed UID in the folder is handled")

	client.AcknowledgeMessages(context.Background(), []string{"INBOX|2"})
	require.Len(saved, 1)
	assert.Equal("INBOX", saved[0].mailbox)
	assert.Equal(client.ObservedFolderStates()["INBOX"], saved[0].state)

	client.AcknowledgeMessages(context.Background(), []string{"Archive|1"})
	require.Len(saved, 2)
	assert.Equal("Archive", saved[1].mailbox)
	assert.Equal(client.ObservedFolderStates()["Archive"], saved[1].state)

	client.AcknowledgeMessages(context.Background(), []string{"INBOX|2"})
	assert.Len(saved, 2, "duplicate acknowledgements must not save a folder twice")
}
