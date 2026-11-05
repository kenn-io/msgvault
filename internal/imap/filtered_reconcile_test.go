package imap

import (
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFilteredReconciliation_CompleteSelectedMailboxes(t *testing.T) {
	for _, all := range []string{"", "[Gmail]/All Mail"} {
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
				if all != "" && excludeAll {
					assert.False(first.ReconcilesFilteredDeletions())
					return
				}
				assert.True(first.ReconcilesFilteredDeletions())
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
