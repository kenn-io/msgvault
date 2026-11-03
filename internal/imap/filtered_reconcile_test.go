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
				assert.False(first.ScannedMailboxSnapshotComplete(), "no listing has completed yet")
				ids := listAllMessages(t, first)
				require.NotEmpty(ids)
				assert.True(first.LabelsSnapshotFiltered())
				assert.False(first.LabelsSnapshotComplete())
				require.True(first.ScannedMailboxSnapshotComplete())
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
				// Saved UIDNEXTs must not turn a scoped run into sparse observations.
				second := newTestClient(t, addr, WithFolderStates(first.ObservedFolderStates()), WithFolderFilter(nil, skipped), WithFilteredDeletionReconciliation(true))
				assert.Equal(ids, listAllMessages(t, second))
				require.True(second.ScannedMailboxSnapshotComplete())
				for _, d := range second.ObservedMailboxDeltas() {
					assert.True(d.Reset)
					assert.Equal([]uint32{1}, d.State.KnownUIDs)
				}
			})
		}
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
				assert.True(client.ScannedMailboxSnapshotComplete())
				assert.NotNil(client.ObservedMailboxDeltas())
				assert.Empty(client.ObservedMailboxDeltas())
			case "configured include":
				assert.True(client.ScannedMailboxSnapshotComplete())
			default:
				assert.False(client.ScannedMailboxSnapshotComplete())
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
			assert.False(t, client.ScannedMailboxSnapshotComplete())
			assert.Nil(t, client.ObservedMailboxDeltas())
			assert.Nil(t, client.ObservedFolderStates())
		})
	}
}
