package cmd

import (
	"fmt"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIMAPLegacyMessageIDFullSyncPersistsMemberships(t *testing.T) {
	for _, header := range []string{"123456789", "<[legacy-token==@example.test]>"} {
		t.Run(header, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			addr, user := testutil.StartIMAPMemServerWithSpecialUse(t,
				map[string]int{"All Mail": 0, "INBOX": 0},
				map[string][]imapapi.MailboxAttr{"All Mail": {imapapi.MailboxAttrAll}},
			)
			raw := []byte(fmt.Sprintf("From: sender@example.test\r\nTo: recipient@example.test\r\n"+
				"Date: Mon, 1 Jan 2024 00:00:00 +0000\r\nMessage-ID: %s\r\n"+
				"Subject: Synthetic legacy identifier\r\n\r\nSynthetic body.\r\n", header))
			for _, mailbox := range []string{"All Mail", "INBOX"} {
				testutil.AppendIMAPRawMessage(t, user, mailbox, raw)
			}
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(sourceTypeIMAP, "imap://legacy@example.test")
			require.NoError(err)
			initial := newScriptedRFC7162Client(t, addr, imaplib.WithFolderFilter([]string{"INBOX"}, nil))
			initialOptions := msgsync.DefaultOptions()
			initialOptions.SourceType = sourceTypeIMAP
			initialSummary, err := newMessageSyncer(initial, st, initialOptions).Full(t.Context(), source.Identifier)
			require.NoError(err)
			require.Zero(initialSummary.Errors)
			require.NoError(initial.Close())

			// After a filtered import, both a full scan and a repeated refresh must
			// publish the same two mailbox memberships for a single archive row.
			for attempt := range 2 {
				client := newScriptedRFC7162Client(t, addr, imapFolderStateOptions(st, source, true)...)
				options := msgsync.DefaultOptions()
				options.SourceType = sourceTypeIMAP
				options.NoResume = true
				summary, err := newMessageSyncer(client, st, options).
					FullWithFinalizer(t.Context(), source, func(summary *gmail.SyncSummary) error {
						return saveIMAPFolderStates(t.Context(), st, source, client, summary, 0)
					})
				require.NoError(err, "sync attempt %d", attempt+1)
				require.Zero(summary.Errors)
				assert.Equal(2, imapMembershipRowCount(t, st, source.ID))

				var count int
				require.NoError(st.DB().QueryRow(st.Rebind(
					`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
				assert.Equal(1, count)
				var allMailID, inboxID int64
				for mailbox, target := range map[string]*int64{"All Mail": &allMailID, "INBOX": &inboxID} {
					require.NoError(st.DB().QueryRow(st.Rebind(
						`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uid = 1`,
					), source.ID, mailbox).Scan(target))
				}
				assert.Equal(allMailID, inboxID)
				storedRaw, err := st.GetMessageRaw(allMailID)
				require.NoError(err)
				assert.Equal(raw, storedRaw)
				states, err := loadIMAPFolderStates(st, source.ID)
				require.NoError(err)
				assert.Len(states, 2)
				require.NoError(client.Close())
			}
		})
	}
}
