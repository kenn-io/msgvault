package imap

import (
	"context"
	"maps"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
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

func TestListMessages_SkipsUnchangedFoldersWithoutModSeq(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 5)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	second := newTestClient(t, addr, WithFolderStates(saved))
	ids := listAllMessages(t, second)
	assert.Empty(ids,
		"UIDNEXT and the message count together prove nothing changed")
	assert.Equal(saved, second.ObservedFolderStates(),
		"skipping still republishes the same complete baseline")

	// Every mailbox must still appear in the delta set: the store retires --
	// and tombstones the messages of -- any mailbox missing from it.
	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 2)
	for _, delta := range deltas {
		assert.False(delta.Reset, delta.Mailbox)
		assert.Empty(delta.ChangedUIDs, delta.Mailbox)
		assert.Empty(delta.VanishedUIDs, delta.Mailbox)
		assert.Equal(saved[delta.Mailbox].KnownUIDs, delta.State.KnownUIDs, delta.Mailbox)
	}
}

func TestListMessages_FetchesOnlyNewMessagesWithoutModSeq(t *testing.T) {
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
		"only the appended message is above the saved high water mark")

	states := second.ObservedFolderStates()
	assert.Equal(uint32(4), states["INBOX"].UIDNext)
	assert.Equal([]uint32{1, 2, 3}, states["INBOX"].KnownUIDs)
	assert.Equal(saved["Archive"], states["Archive"])

	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 2)
	for _, delta := range deltas {
		assert.False(delta.Reset, delta.Mailbox,
			"an additive change must not republish the whole mailbox")
		if delta.Mailbox == "INBOX" {
			assert.Equal([]imapv2.UID{3}, delta.ChangedUIDs)
		} else {
			assert.Empty(delta.ChangedUIDs)
		}
	}
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

	// A resync without usable mod-sequences skips the mailboxes whose UIDNEXT
	// and message count both match the saved baseline.
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

func TestListMessages_AllMailboxUnsupportedQresyncUsesFullFallback(t *testing.T) {
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
	// An \All mailbox deliberately forgoes the per-folder shortcuts: \All may
	// not be a superset of every mailbox, so both the label map and the listing
	// enumerate everything. Folder skipping without QRESYNC is the no-\All
	// path, covered by TestListMessages_SkipsUnchangedFoldersWithoutModSeq.
	assert.ElementsMatch([]string{"All Mail|1", "All Mail|2", "Projects|1"}, ids,
		"an \\All run enumerates every mailbox rather than taking UIDNEXT shortcuts")
	assert.Equal(saved, second.ObservedFolderStates())
	assert.NotNil(second.msgIDToLabels,
		"authoritative fallback must still publish a label map")
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

// TestWithFolderFilter_IncludesOnlySelectedMailboxes verifies that
// WithFolderFilter with an include list limits ListMessages to only
// the specified folders.
func TestWithFolderFilter_IncludesOnlySelectedMailboxes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Provide INBOX, Archive, Trash — we only want Inbox and Trash via filter.
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Trash": 1})

	client := newTestClient(t, addr,
		WithFolderFilter([]string{"INBOX", "Trash"}, nil),
	)

	ids := listAllMessages(t, client)
	// Archive should be excluded; only INBOX and Trash messages.
	assert.Len(ids, 3, "only INBOX and Trash should be listed")

	var inboxCount, trashCount int
	for _, id := range ids {
		if strings.HasPrefix(id, "INBOX|") {
			inboxCount++
		}
		if strings.HasPrefix(id, "Trash|") {
			trashCount++
		}
		assert.True(strings.HasPrefix(id, "INBOX") || strings.HasPrefix(id, "Trash"), "unexpected mailbox in results: %s", id)
	}
	assert.Equal(2, inboxCount, "INBOX messages")
	assert.Equal(1, trashCount, "Trash messages")

	// Archive should not appear.
	for _, id := range ids {
		assert.False(strings.HasPrefix(id, "Archive|"), "Archive should not be listed")
	}

	// Folder states recorded should only include the filtered mailboxes.
	states := client.ObservedFolderStates()
	require.Contains(states, "INBOX")
	require.Contains(states, "Trash")
	assert.NotContains(states, "Archive", "Archive state should not be recorded")
}

// TestWithFolderFilter_ExcludesSpecifiedMailboxes verifies that
// WithFolderFilter with an exclude list removes the specified
// mailboxes from ListMessages.
func TestWithFolderFilter_ExcludesSpecifiedMailboxes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Trash": 1})

	client := newTestClient(t, addr,
		WithFolderFilter(nil, []string{"Trash"}),
	)

	ids := listAllMessages(t, client)
	require.Len(ids, 5, "INBOX(2) + Archive(3) = 5 after excluding Trash(1)")

	for _, id := range ids {
		assert.False(strings.HasPrefix(id, "Trash|"), "Trash should not be listed")
	}

	states := client.ObservedFolderStates()
	require.Contains(states, "INBOX")
	require.Contains(states, "Archive")
	assert.NotContains(states, "Trash", "Trash state should not be recorded")
}

// TestWithFolderFilter_ConfigFoldersRespectsIncludeList verifies that
// the Folders field in the IMAP config is applied during
// buildMessageListCache via filterMailboxes before any CLI filter would.
func TestWithFolderFilter_ConfigFoldersRespectsIncludeList(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Drafts": 1})

	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portStr)
	require.NoError(err)

	// Configure the client with Folders = ["Archive"] so only Archive
	// should appear during ListMessages.
	client := NewClient(&Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
		Folders:  []string{"Archive"},
	}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })

	ids := listAllMessages(t, client)
	require.Len(ids, 3, "only messages from Archive should be listed")

	// All results must be from Archive.
	for _, id := range ids {
		assert.True(strings.HasPrefix(id, "Archive|"), "expected Archive mailbox, got: %s", id)
	}

	states := client.ObservedFolderStates()
	require.Contains(states, "Archive")
	assert.NotContains(states, "INBOX")
	assert.NotContains(states, "Drafts")
}

// TestWithFolderFilter_CLIReplacingConfig verifies that a CLI --folder
// include filter fully replaces the config's include list when set.
// CLI exclusions apply on top of the effective include regardless.
func TestWithFolderFilter_CLIReplacingConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Case 1: CLI include replaces config include — config says
	// Archive, --folder says INBOX, expect only INBOX.
	t.Run("CLI include replaces config include", func(t *testing.T) {
		addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Trash": 1})

		host, portStr, err := net.SplitHostPort(addr)
		require.NoError(err)
		port, err := strconv.Atoi(portStr)
		require.NoError(err)

		client := NewClient(&Config{
			Host:     host,
			Port:     port,
			Username: testutil.IMAPTestUsername,
			Folders:  []string{"Archive"},
		}, testutil.IMAPTestPassword,
			WithFolderFilter([]string{"INBOX"}, nil),
		)
		t.Cleanup(func() { _ = client.Close() })

		ids := listAllMessages(t, client)
		assert.Len(ids, 2, "CLI include replaces config include — only INBOX listed")

		for _, id := range ids {
			assert.True(strings.HasPrefix(id, "INBOX|"), "expected INBOX, got: %s", id)
		}
	})

	// Case 2: CLI include + CLI exclude applied together — config
	// includes nothing, --folder=["Inbox,Archive"],
	// --skip-folder=["Inbox"]. Expect only Archive.
	t.Run("CLI include and CLI exclude combine", func(t *testing.T) {
		addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Trash": 1})

		host, portStr, err := net.SplitHostPort(addr)
		require.NoError(err)
		port, err := strconv.Atoi(portStr)
		require.NoError(err)

		client := NewClient(&Config{
			Host:     host,
			Port:     port,
			Username: testutil.IMAPTestUsername,
		}, testutil.IMAPTestPassword,
			WithFolderFilter([]string{"Inbox", "Archive"}, []string{"Inbox"}),
		)
		t.Cleanup(func() { _ = client.Close() })

		ids := listAllMessages(t, client)
		require.Len(ids, 3, "include Inbox+Archive minus exclude Inbox = Archive only")

		for _, id := range ids {
			assert.True(strings.HasPrefix(id, "Archive|"), "expected Archive, got: %s", id)
		}
	})
}

// TestWithFolderFilter_CLIExcludeWithConfigIncludes verifies that when
// only --skip-folder is set (CLI exclude), it excludes from the config's
// include list. With no config Folders set, --skip-folder excludes from
// all mailboxes.
func TestWithFolderFilter_CLIExcludeConfigIncludes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Trash": 1})

	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portStr)
	require.NoError(err)

	// Config includes Archive, CLI excludes Trash.
	// Result: Archive - Trash∩Archive (nothing) = Archive only.
	client := NewClient(&Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
		Folders:  []string{"Archive"},
	}, testutil.IMAPTestPassword,
		WithFolderFilter(nil, []string{"Trash"}),
	)
	t.Cleanup(func() { _ = client.Close() })

	ids := listAllMessages(t, client)
	// Config says Archive (2 messages), Trash is excluded but doesn't overlap.
	require.Len(ids, 3, "Archive only (Trash excluded, config doesn't include Trash)")

	for _, id := range ids {
		assert.True(strings.HasPrefix(id, "Archive|"), "expected Archive, got: %s", id)
	}

	// Now test CLI exclude with no config folders — exclude from ALL.
	addr2, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Trash": 1})
	host2, portStr2, err := net.SplitHostPort(addr2)
	require.NoError(err)
	port2, err := strconv.Atoi(portStr2)
	require.NoError(err)

	client2 := NewClient(&Config{
		Host:     host2,
		Port:     port2,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword,
		WithFolderFilter(nil, []string{"Archive"}),
	)
	t.Cleanup(func() { _ = client2.Close() })

	ids2 := listAllMessages(t, client2)
	require.Len(ids2, 3, "all minus Archive (INBOX(2) + Trash(1))")

	for _, id := range ids2 {
		assert.False(strings.HasPrefix(id, "Archive|"), "Archive should not appear when excluded with no config")
	}
}

// TestWithFolderFilter_NoConfigNoCLIIncludesAll verifies that when
// neither config folders nor CLI folder filters are set, all mailboxes
// are listed (no-op filter).
func TestWithFolderFilter_NoConfigNoCLIIncludesAll(t *testing.T) {
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3, "Drafts": 1})

	client := newTestClient(t, addr)
	ids := listAllMessages(t, client)
	assert.Len(t, ids, 6, "without filters all mailboxes should list")
}

func TestClientReportsLabelSnapshotCompleteness(t *testing.T) {
	after := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, time.February, 3, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		cfg          *Config
		opts         []Option
		wantComplete bool
		wantFiltered bool
	}{
		{
			name:         "unfiltered",
			cfg:          &Config{},
			wantComplete: true,
		},
		{
			name:         "config include",
			cfg:          &Config{Folders: []string{"INBOX"}},
			wantFiltered: true,
		},
		{
			name:         "runtime include",
			cfg:          &Config{},
			opts:         []Option{WithFolderFilter([]string{"INBOX"}, nil)},
			wantFiltered: true,
		},
		{
			name:         "runtime exclude",
			cfg:          &Config{},
			opts:         []Option{WithFolderFilter(nil, []string{"Trash"})},
			wantFiltered: true,
		},
		{
			name:         "after filter",
			cfg:          &Config{},
			opts:         []Option{WithDateFilter(after, time.Time{})},
			wantFiltered: true,
		},
		{
			name:         "before filter",
			cfg:          &Config{},
			opts:         []Option{WithDateFilter(time.Time{}, before)},
			wantFiltered: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			client := NewClient(tt.cfg, "", tt.opts...)
			completeness, ok := any(client).(interface {
				LabelsSnapshotComplete() bool
			})
			require.True(ok, "IMAP client must report label snapshot completeness")
			assert.Equal(tt.wantComplete, completeness.LabelsSnapshotComplete())

			filtering, ok := any(client).(interface {
				LabelsSnapshotFiltered() bool
			})
			require.True(ok, "IMAP client must report explicit snapshot filters")
			assert.Equal(tt.wantFiltered, filtering.LabelsSnapshotFiltered())
		})
	}
}

func TestSourceMessageExists(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	client := newTestClient(t, addr)

	exists, err := client.SourceMessageExists(
		context.Background(), "INBOX|1")
	require.NoError(err)
	require.True(exists)

	exists, err = client.SourceMessageExists(
		context.Background(), "INBOX|99")
	require.NoError(err)
	require.False(exists)

	require.Equal([]string{"INBOX|1"}, listAllMessages(t, client))

	exists, err = client.SourceMessageExists(
		context.Background(), "Renamed|1")
	require.NoError(err)
	require.False(exists)

	_, err = client.SourceMessageExists(context.Background(), "invalid")
	require.Error(err)
}

func TestSourceMessageExistsPreservesListedMailboxSelectionError(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServerWithSelectError(
		t,
		map[string]int{"INBOX": 1},
		nil,
		"INBOX",
	)
	client := newTestClient(t, addr)

	require.Empty(listAllMessages(t, client))
	require.Equal([]string{"INBOX"}, client.mailboxCache)

	_, err := client.SourceMessageExists(
		context.Background(), "INBOX|1")
	require.Error(err)
}

func TestSourceMessageMatchesDefersExcludedMailbox(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServerWithSelectError(
		t,
		map[string]int{
			"Archive": 1,
			"INBOX":   1,
		},
		nil,
		"Archive",
	)
	client := newTestClient(
		t, addr, WithFolderFilter([]string{"INBOX"}, nil))

	require.Equal([]string{"INBOX|1"}, listAllMessages(t, client))

	matches, conclusive, err := client.SourceMessageMatches(
		context.Background(), "Archive|1", "excluded@example.com")
	require.NoError(err)
	assert.False(matches)
	assert.False(conclusive)
}

func TestSourceMessageMatchesRFC822Identity(t *testing.T) {
	require := require.New(t)
	addr, user := testutil.StartIMAPMemServer(
		t, map[string]int{"INBOX": 0})
	const messageID = "source-match@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	client := newTestClient(t, addr)
	require.Equal([]string{"INBOX|1"}, listAllMessages(t, client))

	matches, conclusive, err := client.SourceMessageMatches(
		context.Background(), "INBOX|1", messageID)
	require.NoError(err)
	require.True(matches)
	require.True(conclusive)

	matches, conclusive, err = client.SourceMessageMatches(
		context.Background(), "INBOX|1", "<"+messageID+">")
	require.NoError(err)
	require.True(matches)
	require.True(conclusive)

	matches, conclusive, err = client.SourceMessageMatches(
		context.Background(), "INBOX|1", "replacement@example.com")
	require.NoError(err)
	require.False(matches)
	require.True(conclusive)
}

func TestSourceMessageMatchesRejectsUIDValidityChange(t *testing.T) {
	require := require.New(t)
	addr, user := testutil.StartIMAPMemServer(
		t, map[string]int{"INBOX": 0})
	const messageID = "uidvalidity-match@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)

	first := newTestClient(t, addr)
	require.Equal([]string{"INBOX|1"}, listAllMessages(t, first))
	stale := first.ObservedFolderStates()
	require.NoError(first.Close())
	state := stale["INBOX"]
	state.UIDValidity++
	stale["INBOX"] = state

	second := newTestClient(t, addr, WithFolderStates(stale))
	require.Equal([]string{"INBOX|1"}, listAllMessages(t, second))
	matches, conclusive, err := second.SourceMessageMatches(
		context.Background(), "INBOX|1", messageID)
	require.NoError(err)
	require.False(matches)
	require.True(conclusive)
}

func TestFetchedSourceMessageMatchesIdentityMatrix(t *testing.T) {
	addr, user := testutil.StartIMAPMemServer(
		t, map[string]int{"INBOX": 0, "Removed": 0})
	const messageID = "fetched-source@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	first := newTestClient(t, addr)
	require.Equal(t, []string{"INBOX|1"}, listAllMessages(t, first))
	saved := first.ObservedFolderStates()
	require.NoError(t, first.Close())
	require.NoError(t, user.Delete("Removed"))

	changedEpoch := make(map[string]FolderState, len(saved))
	maps.Copy(changedEpoch, saved)
	state := changedEpoch["INBOX"]
	state.UIDValidity++
	changedEpoch["INBOX"] = state

	tests := []struct {
		name       string
		opts       []Option
		sourceID   string
		expectedID string
		actualID   string
		wantMatch  bool
		conclusive bool
		wantErr    bool
	}{
		{
			name:       "same epoch with both IDs missing",
			opts:       []Option{WithFolderStates(saved)},
			sourceID:   "INBOX|1",
			wantMatch:  true,
			conclusive: true,
		},
		{
			name:       "changed epoch with both IDs missing",
			opts:       []Option{WithFolderStates(changedEpoch)},
			sourceID:   "INBOX|1",
			conclusive: true,
		},
		{
			name:       "missing epoch with both IDs missing",
			sourceID:   "INBOX|1",
			conclusive: false,
		},
		{
			name:       "removed mailbox",
			opts:       []Option{WithFolderStates(saved)},
			sourceID:   "Removed|1",
			conclusive: true,
		},
		{
			name:       "equal nonempty IDs normalize angle brackets",
			sourceID:   "INBOX|1",
			expectedID: "<" + messageID + ">",
			actualID:   messageID,
			wantMatch:  true,
			conclusive: true,
		},
		{
			name:       "unequal nonempty IDs",
			sourceID:   "INBOX|1",
			expectedID: "old@example.com",
			actualID:   messageID,
			conclusive: true,
		},
		{
			name:       "only archived ID missing without epoch",
			sourceID:   "INBOX|1",
			actualID:   messageID,
			conclusive: false,
		},
		{
			name:       "only fetched ID missing without epoch",
			sourceID:   "INBOX|1",
			expectedID: messageID,
			conclusive: false,
		},
		{
			name:       "only archived ID missing in same epoch",
			opts:       []Option{WithFolderStates(saved)},
			sourceID:   "INBOX|1",
			actualID:   messageID,
			wantMatch:  true,
			conclusive: true,
		},
		{
			name:       "only fetched ID missing in same epoch",
			opts:       []Option{WithFolderStates(saved)},
			sourceID:   "INBOX|1",
			expectedID: messageID,
			wantMatch:  true,
			conclusive: true,
		},
		{
			name:     "malformed composite ID",
			sourceID: "not-composite",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			client := newTestClient(t, addr, tt.opts...)
			listAllMessages(t, client)

			matches, conclusive, err := client.FetchedSourceMessageMatches(
				tt.sourceID,
				tt.expectedID,
				tt.actualID,
			)
			if tt.wantErr {
				require.Error(err)
				return
			}
			require.NoError(err)
			assert.Equal(tt.wantMatch, matches)
			assert.Equal(tt.conclusive, conclusive)
		})
	}
}

func TestClientReportsCompleteSnapshotWithoutAllMailboxOnFullScan(t *testing.T) {
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{
		"INBOX":   1,
		"Archive": 1,
	})
	client := newTestClient(t, addr)

	require.Len(t, listAllMessages(t, client), 2)

	assert.True(t, client.LabelsSnapshotComplete(),
		"a fully enumerated server has an authoritative membership snapshot")
}

func TestFullScanLabelMapRecordsEveryMailboxMembership(t *testing.T) {
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"INBOX":   0,
		"Archive": 0,
	})
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", "inbox-membership@example.com")
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", "archive-membership@example.com")
	client := newTestClient(t, addr)

	require.Len(t, listAllMessages(t, client), 2)
	observed := client.ObservedMemberships()
	assert.ElementsMatch(t, []MembershipObservation{
		{
			Mailbox: "INBOX", UIDValidity: client.ObservedFolderStates()["INBOX"].UIDValidity,
			UID: 1, SourceMessageID: "INBOX|1", RFC822MessageID: "inbox-membership@example.com",
			Flags: []string{},
		},
		{
			Mailbox: "Archive", UIDValidity: client.ObservedFolderStates()["Archive"].UIDValidity,
			UID: 1, SourceMessageID: "Archive|1", RFC822MessageID: "archive-membership@example.com",
			Flags: []string{},
		},
	}, observed)
}

func TestGmailAllMailFullScanPublishesDeltaForLabelOnlyMailbox(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{
			"INBOX":            0,
			"[Gmail]/All Mail": 0,
		},
		map[string][]imapv2.MailboxAttr{
			"[Gmail]/All Mail": {imapv2.MailboxAttrAll},
		},
	)
	const messageID = "gmail-overlap@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "[Gmail]/All Mail", messageID)
	client := newTestClient(t, addr)

	require.Equal([]string{"[Gmail]/All Mail|1"}, listAllMessages(t, client))
	deltas := client.ObservedMailboxDeltas()
	require.Len(deltas, 2, "the label-map-only mailbox needs a durable baseline delta")
	assert.ElementsMatch([]string{"INBOX", "[Gmail]/All Mail"}, []string{
		deltas[0].Mailbox,
		deltas[1].Mailbox,
	})
	client.mu.Lock()
	supportsModSeq := client.conn.Caps().Has(imapv2.CapCondStore)
	client.mu.Unlock()
	for _, delta := range deltas {
		assert.True(delta.Reset)
		assert.NotZero(delta.State.UIDValidity)
		assert.Equal(uint32(2), delta.State.UIDNext)
		assert.Equal([]uint32{1}, delta.State.KnownUIDs)
		if supportsModSeq {
			assert.NotZero(delta.State.HighestModSeq)
		}
	}
}

func TestGmailAllMailStatusFailureSuppressesIncrementalPublication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServerWithStatusError(
		t,
		map[string]int{
			"INBOX":            0,
			"[Gmail]/All Mail": 0,
		},
		map[string][]imapv2.MailboxAttr{
			"[Gmail]/All Mail": {imapv2.MailboxAttrAll},
		},
		"INBOX",
	)
	const messageID = "status-failure@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "[Gmail]/All Mail", messageID)
	client := newTestClient(t, addr)

	require.Equal([]string{"[Gmail]/All Mail|1"}, listAllMessages(t, client))
	assert.False(client.LabelsSnapshotComplete())
	assert.Nil(client.ObservedFolderStates())
	assert.Nil(client.ObservedMailboxDeltas(),
		"a missing STATUS must not expose a zero-valued mailbox cursor")
}

func TestClientRebuildsCompleteSnapshotWithoutAllMailboxOrModSeq(t *testing.T) {
	require := require.New(t)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"INBOX":   1,
		"Archive": 1,
	})
	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 2)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	testutil.AppendIMAPMessage(t, user, "Archive")
	second := newTestClient(t, addr, WithFolderStates(saved))
	require.Equal([]string{"Archive|2"}, listAllMessages(t, second))

	assert.True(t, second.LabelsSnapshotComplete(),
		"skipping unchanged mailboxes still yields an authoritative snapshot")
}

func TestClientReportsIncompleteMailboxMembershipCollection(t *testing.T) {
	addr, _ := testutil.StartIMAPMemServerWithSelectError(
		t,
		map[string]int{
			"All Mail": 1,
			"Archive":  1,
		},
		map[string][]imapv2.MailboxAttr{
			"All Mail": {imapv2.MailboxAttrAll},
		},
		"Archive",
	)
	client := newTestClient(t, addr)

	require.Equal(t, []string{"All Mail|1"}, listAllMessages(t, client))

	assert.False(t, client.LabelsSnapshotComplete(),
		"a failed mailbox membership scan must not be authoritative")
}

func TestWithFolderFilter_GmailCanonicalMailboxesHonorFilters(t *testing.T) {
	startServer := func(t *testing.T) string {
		t.Helper()
		addr, _ := testutil.StartIMAPMemServerWithSpecialUse(
			t,
			map[string]int{
				"INBOX":            2,
				"[Gmail]/All Mail": 3,
				"[Gmail]/Trash":    1,
				"[Gmail]/Spam":     1,
			},
			map[string][]imapv2.MailboxAttr{
				"[Gmail]/All Mail": {imapv2.MailboxAttrAll},
				"[Gmail]/Trash":    {imapv2.MailboxAttrTrash},
				"[Gmail]/Spam":     {imapv2.MailboxAttrJunk},
			},
		)
		return addr
	}

	t.Run("include only INBOX", func(t *testing.T) {
		client := newTestClient(t, startServer(t),
			WithFolderFilter([]string{"INBOX"}, nil))

		ids := listAllMessages(t, client)
		require.Len(t, ids, 2)
		for _, id := range ids {
			assert.True(t, strings.HasPrefix(id, "INBOX|"), "unexpected message ID %q", id)
		}
	})

	t.Run("exclude Trash", func(t *testing.T) {
		client := newTestClient(t, startServer(t),
			WithFolderFilter(nil, []string{"[Gmail]/Trash"}))

		ids := listAllMessages(t, client)
		require.NotEmpty(t, ids)
		for _, id := range ids {
			assert.False(t, strings.HasPrefix(id, "[Gmail]/Trash|"), "Trash must not be enumerated: %q", id)
		}
	})
}

// TestListMessages_ExpungeVanishesWithoutRepublishing covers the case UIDNEXT
// alone gets wrong: a message removed with nothing appended leaves the high
// water mark exactly where it was, so only the message count reveals the
// change. Finding out costs a full UID SEARCH, but the search answers with a
// diff -- the mailbox does not have to be republished to retire one UID.
func TestListMessages_ExpungeVanishesWithoutRepublishing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 3, "Archive": 2})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 5)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	testutil.ExpungeIMAPMessage(t, addr, "INBOX", 1)

	second := newTestClient(t, addr, WithFolderStates(saved))
	assert.Empty(listAllMessages(t, second),
		"an expunge adds no messages, so none may be re-listed")

	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 2)
	for _, delta := range deltas {
		if delta.Mailbox != "INBOX" {
			assert.False(delta.Reset, "an untouched mailbox stays untouched")
			continue
		}
		assert.False(delta.Reset,
			"a known baseline makes the removal expressible without a republish")
		assert.Equal([]imapv2.UID{1}, delta.VanishedUIDs)
		assert.Empty(delta.ChangedUIDs)
		assert.Equal([]uint32{2, 3}, delta.State.KnownUIDs)
	}
}

// TestListMessages_ExpungeWithoutBaselineStillResets covers the same expunge
// with no stored KnownUIDs to diff against: there is nothing to subtract from,
// so a full republish remains the only correct answer.
func TestListMessages_ExpungeWithoutBaselineStillResets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 3})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 3)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	// A state saved before memberships were tracked carries no UID baseline.
	for mailbox, state := range saved {
		state.KnownUIDs = nil
		saved[mailbox] = state
	}
	testutil.ExpungeIMAPMessage(t, addr, "INBOX", 1)

	second := newTestClient(t, addr, WithFolderStates(saved))
	assert.Equal([]string{"INBOX|2", "INBOX|3"}, listAllMessages(t, second))

	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 1)
	assert.True(deltas[0].Reset,
		"without a baseline only a full republish can retire the expunged UID")
	assert.Empty(deltas[0].VanishedUIDs)
	assert.Equal([]uint32{2, 3}, deltas[0].State.KnownUIDs)
}

// TestListMessages_AdvancedUIDNextWithSteadyCountDoesNotReset covers a message
// that arrived and was expunged between syncs: UIDNEXT moved but the mailbox
// still holds the same messages, so nothing needs republishing.
//
// This does not exercise the RFC 3501 6.4.8 boundary rule -- the in-memory
// server resolves "*" to UIDNEXT-1 rather than the last message, so the search
// comes back empty here. TestListMessagesToleratesBoundaryUIDAboveHighWaterMark
// covers that against a scripted server.
func TestListMessages_AdvancedUIDNextWithSteadyCountDoesNotReset(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 2)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	// Append then expunge the same message: UIDNEXT advances, the count does
	// not, and the search above the high water mark finds only old UID 2.
	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.ExpungeIMAPMessage(t, addr, "INBOX", 3)

	second := newTestClient(t, addr, WithFolderStates(saved))
	assert.Empty(listAllMessages(t, second),
		"a message that came and went leaves nothing to archive")

	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 1)
	assert.False(deltas[0].Reset,
		"an advanced UIDNEXT with a steady count is not a deletion")
	assert.Equal([]uint32{1, 2}, deltas[0].State.KnownUIDs)
}

func TestListMessages_FullyEnumeratesWithoutKnownUIDBaseline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 2)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	// A cursor saved before the membership table existed: the counts would
	// match a zero-length baseline, but nothing is actually known.
	stale := map[string]FolderState{"INBOX": {
		UIDValidity: saved["INBOX"].UIDValidity,
		UIDNext:     saved["INBOX"].UIDNext,
	}}
	require.Nil(stale["INBOX"].KnownUIDs)

	second := newTestClient(t, addr, WithFolderStates(stale))
	assert.Len(listAllMessages(t, second), 2,
		"a missing baseline must be rebuilt, not assumed empty")
	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 1)
	assert.True(deltas[0].Reset)
}

func TestListMessages_LimitedSyncDisablesFolderSkipping(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})

	first := newTestClient(t, addr)
	require.Len(listAllMessages(t, first), 5)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())

	// A limited sync cannot defer label reconciliation, so it must see every
	// mailbox membership rather than trusting the stored ones.
	second := newTestClient(t, addr, WithFolderStates(saved))
	second.ForceFullEnumerationForLimitedSync()
	assert.Len(listAllMessages(t, second), 5,
		"a limited sync must not take folder shortcuts")
}
