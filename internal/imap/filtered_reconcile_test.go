package imap

import (
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFilteredReconciliation_CompleteSelectedMailboxes(t *testing.T) {
	for _, all := range []string{"", "[Gmail]/All Mail", "All Mail"} {
		for _, excludeAll := range []bool{false, true} {
			t.Run(all+map[bool]string{true: " excluded", false: " selected"}[excludeAll], func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				counts := map[string]int{"INBOX": 0, "Trash": 1}
				roles := map[string][]imapv2.MailboxAttr{}
				skipped := []string{"Trash"}
				if all != "" {
					counts[all] = 0
					roles[all] = []imapv2.MailboxAttr{imapv2.MailboxAttrAll}
					if excludeAll {
						skipped = append(skipped, all)
					}
				}
				addr, user := testutil.StartIMAPMemServerWithSpecialUse(t, counts, roles)
				testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", "overlap@example.com")
				if all != "" {
					testutil.AppendIMAPMessageWithMessageID(t, user, all, "overlap@example.com")
				}
				first := newTestClient(t, addr, WithFolderFilter(nil, skipped), WithFilteredDeletionReconciliation(true))
				ids := listAllMessages(t, first)
				require.NotEmpty(ids)
				assert.True(first.LabelsSnapshotFiltered())
				assert.False(first.LabelsSnapshotComplete())
				require.NotNil(first.ObservedMailboxDeltas())
				want := []string{"INBOX"}
				if all != "" && !excludeAll {
					want = append(want, all)
				}
				var got []string
				for _, d := range first.ObservedMailboxDeltas() {
					got = append(got, d.Mailbox)
					assert.True(d.Reset)
				}
				assert.ElementsMatch(want, got)
				assert.NotContains(first.ObservedFolderStates(), "Trash")
			})
		}
	}
}

func TestFilteredReconciliation_SavedBaseline(t *testing.T) {
	for _, expunge := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "expunge"}[expunge], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 3, "Trash": 1})
			first := newTestClient(t, addr, WithFolderFilter(nil, []string{"Trash"}), WithFilteredDeletionReconciliation(true))
			require.Len(listAllMessages(t, first), 3)
			saved := first.ObservedFolderStates()
			require.NoError(first.Close())
			if expunge {
				testutil.ExpungeIMAPMessage(t, addr, "INBOX", 2)
			}
			second := newTestClient(t, addr, WithFolderStates(saved), WithFolderFilter(nil, []string{"Trash"}), WithFilteredDeletionReconciliation(true))
			assert.Empty(listAllMessages(t, second), "surviving messages need no header fetch or reprocessing")
			assert.Empty(second.ObservedMemberships())
			deltas := second.ObservedMailboxDeltas()
			require.Len(deltas, 1)
			delta := deltas[0]
			assert.Equal("INBOX", delta.Mailbox)
			assert.False(delta.Reset)
			assert.Empty(delta.ChangedUIDs)
			if expunge {
				assert.Equal([]imapv2.UID{2}, delta.VanishedUIDs)
				assert.Equal([]uint32{1, 3}, delta.State.KnownUIDs)
			} else {
				assert.Empty(delta.VanishedUIDs)
				assert.Equal([]uint32{1, 2, 3}, delta.State.KnownUIDs)
				assert.Equal(saved, second.ObservedFolderStates())
			}
		})
	}
}

func TestFilteredReconciliation_DefaultDatesAndEmptySelection(t *testing.T) {
	for _, name := range []string{"default", "since", "before", "empty", "configured include"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1, "Trash": 1})
			opts := []Option{WithFolderFilter(nil, []string{"Trash"})}
			if name != "default" {
				opts = append(opts, WithFilteredDeletionReconciliation(true))
			}
			switch name {
			case "since":
				opts = append(opts, WithDateFilter(time.Now().AddDate(-1, 0, 0), time.Time{}))
			case "before":
				opts = append(opts, WithDateFilter(time.Time{}, time.Now().AddDate(1, 0, 0)))
			case "empty":
				opts = append(opts, WithFolderFilter([]string{"Absent"}, nil))
			}
			client := newTestClient(t, addr, opts...)
			if name == "configured include" {
				client.config.Folders = []string{"INBOX"}
				client.folderFilterExclude = nil
			}
			listAllMessages(t, client)
			switch name {
			case "empty":
				assert.True(client.ReconcilesFilteredDeletions())
				assert.NotNil(client.ObservedMailboxDeltas())
				assert.Empty(client.ObservedMailboxDeltas())
			case "configured include":
				assert.True(client.ReconcilesFilteredDeletions())
			default:
				assert.False(client.ReconcilesFilteredDeletions())
			}
			assert.False(client.LabelsSnapshotComplete())
		})
	}
}

func TestFilteredReconciliation_IncompleteMailboxSuppressesSnapshot(t *testing.T) {
	for _, name := range []string{"status", "select", "fetch"} {
		t.Run(name, func(t *testing.T) {
			counts := map[string]int{"INBOX": 1, "Archive": 1, "Trash": 1}
			var addr string
			switch name {
			case "status":
				addr, _ = testutil.StartIMAPMemServerWithStatusError(t, counts, nil, "Archive")
			case "select":
				addr, _ = testutil.StartIMAPMemServerWithSelectError(t, counts, nil, "Archive")
			case "fetch":
				addr, _ = testutil.StartIMAPMemServerOmittingUIDFromEveryFetch(t, counts, "Archive", 1)
			}
			client := newTestClient(t, addr, WithFolderFilter(nil, []string{"Trash"}), WithFilteredDeletionReconciliation(true))
			listAllMessages(t, client)
			assert.Nil(t, client.ObservedMailboxDeltas())
			assert.Nil(t, client.ObservedFolderStates())
		})
	}
}

func TestFilteredReconciliation_AbsentSkippedMailboxKeepsMembership(t *testing.T) {
	for _, mode := range []string{"renamed exclusion", "outside saved allow list"} {
		t.Run(mode, func(t *testing.T) {
			addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0, "Trash": 1})
			first := newTestClient(t, addr)
			listAllMessages(t, first)
			saved := first.ObservedFolderStates()
			require.NoError(t, first.Close())
			require.NoError(t, user.Rename("Trash", "Renamed", nil))
			client := newTestClient(t, addr, WithFolderStates(saved), WithFolderFilter(nil, []string{"Trash", "Renamed"}), WithFilteredDeletionReconciliation(true))
			if mode == "outside saved allow list" {
				client.config.Folders = []string{"INBOX"}
				client.folderFilterExclude = nil
			}
			listAllMessages(t, client)
			assert.Contains(t, client.ListedMailboxes(), "Trash")
			assert.Contains(t, client.ListedMailboxes(), "Renamed")
		})
	}
}
