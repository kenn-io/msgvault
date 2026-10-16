package sync

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncLabelsPreservesArchivedRoles(t *testing.T) {
	for _, test := range []struct {
		name, sourceType, historyName, historyRole, nextName, nextRole             string
		rename, repeatCollision, subset, coalesced, clearCoalesced, chain, upgrade bool
	}{
		{name: "new ordinary folder", sourceType: "imap", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Sent"},
		{name: "renamed ordinary Gmail label", sourceType: "gmail", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Sent", rename: true},
		{name: "generated archive name claimed later", sourceType: "imap", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Sent", repeatCollision: true},
		{name: "subset retains archive distinction", sourceType: "imap", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Sent", subset: true},
		{name: "later merge keeps archived role separate", sourceType: "imap", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Sent", chain: true},
		{name: "upgrade creates durable label storage", sourceType: "imap", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Sent", upgrade: true},
		{name: "Drafts role survives ordinary folder", sourceType: "imap", historyName: "Drafts", historyRole: store.LabelSystemRoleDrafts, nextName: "Drafts"},
		{name: "unrelated ordinary folder", sourceType: "imap", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Archive"},
		{name: "new same-role folder remains separate", sourceType: "imap", historyName: "Sent", historyRole: store.LabelSystemRoleSent, nextName: "Sent", nextRole: store.LabelSystemRoleSent},
		{name: "coalesced trusted Sent", sourceType: "imap", historyName: "Sent Items", historyRole: store.LabelSystemRoleSent, nextName: "Sent", coalesced: true},
		{name: "coalesced live role can still clear", sourceType: "imap", historyName: "Sent Items", historyRole: store.LabelSystemRoleSent, nextName: "Sent Items", coalesced: true, clearCoalesced: true},
		{name: "ordinary display name remains adoptable", sourceType: "imap", historyName: "Sent", nextName: "Sent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			if test.subset {
				testutil.SkipIfPostgres(t, "subset copies SQLite archives")
			}
			st := testutil.NewTestStore(t)
			if test.upgrade {
				_, err := st.DB().Exec(`DROP TABLE IF EXISTS source_merge_preserved_labels`)
				require.NoError(err)
				require.NoError(st.InitSchemaContext(t.Context()))
				require.NoError(st.InitSchemaContext(t.Context()))
			}
			live, err := st.GetOrCreateSource(test.sourceType, "live@example.test")
			require.NoError(err)
			history, err := st.GetOrCreateSource(test.sourceType, "history@example.test")
			require.NoError(err)
			require.NoError(st.AddAccountIdentity(live.ID, "live@example.test", "manual"))
			require.NoError(st.AddAccountIdentity(history.ID, "history@example.test", "manual"))
			liveSentID := "Sent Items"
			if test.sourceType == "gmail" {
				liveSentID = "SENT"
			}
			api := &staticLabelsAPI{MockAPI: gmail.NewMockAPI(), labels: []*gmail.Label{
				{ID: liveSentID, Name: "Sent Items", Type: "system", SystemRole: store.LabelSystemRoleSent},
			}}
			if test.rename {
				api.labels = append(api.labels, &gmail.Label{ID: "Label_20", Name: "Projects", Type: "user"})
			}
			if test.clearCoalesced {
				// Keep another trusted folder so sender fallback cannot mask role clearing.
				api.labels = append(api.labels, &gmail.Label{ID: "Other Sent", Name: "Other Sent", Type: "system", SystemRole: store.LabelSystemRoleSent})
			}
			syncer := New(api, st, &Options{SourceType: test.sourceType})
			liveLabels, err := syncer.syncLabels(t.Context(), live.ID)
			require.NoError(err)
			historyDescriptors := map[string]store.LabelInfo{
				test.historyName: {Name: test.historyName, Type: "system", SystemRole: test.historyRole},
			}
			if test.historyRole == "" {
				historyDescriptors["Other Sent"] = store.LabelInfo{Name: "Other Sent", Type: "system", SystemRole: store.LabelSystemRoleSent}
			}
			historyLabels, err := st.EnsureLabelsBatch(history.ID, historyDescriptors)
			require.NoError(err)
			conversation, err := st.EnsureConversation(history.ID, "historical-thread", "Historical thread")
			require.NoError(err)
			sender, err := st.EnsureParticipant("history@example.test", "", "example.test")
			require.NoError(err)
			recipient, err := st.EnsureParticipant("friend@example.test", "", "example.test")
			require.NoError(err)
			id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message:  &store.Message{SourceID: history.ID, ConversationID: conversation, SourceMessageID: "historical-message", MessageType: "email"},
				RawMIME:  []byte("From: history@example.test\r\nTo: friend@example.test\r\n\r\nhistorical body"),
				LabelIDs: []int64{historyLabels[test.historyName]},
				Recipients: []store.RecipientSet{
					{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{""}, EmailAddresses: []string{"history@example.test"}},
					{Type: "to", ParticipantIDs: []int64{recipient}, DisplayNames: []string{""}, EmailAddresses: []string{"friend@example.test"}},
				},
			})
			require.NoError(err)
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
			require.NoError(err)
			labelIDs, err := st.MessageLabelIDsContext(t.Context(), id)
			require.NoError(err)
			require.Len(labelIDs, 1)
			archiveID := labelIDs[0]
			if test.coalesced {
				require.Equal(liveLabels[liveSentID], archiveID)
			} else {
				require.Equal(historyLabels[test.historyName], archiveID)
			}
			var roleBefore sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT system_role FROM labels WHERE id = ?`), archiveID).Scan(&roleBefore))
			require.Equal(test.historyRole, roleBefore.String)

			if test.subset {
				var sequence int
				var database, sourcePath string
				require.NoError(st.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &database, &sourcePath))
				destination := filepath.Join(t.TempDir(), "subset")
				_, err = store.CopySubset(sourcePath, destination, 1, false)
				require.NoError(err)
				subset, err := store.OpenForTest(filepath.Join(destination, "msgvault.db"))
				require.NoError(err)
				t.Cleanup(func() { require.NoError(subset.Close()) })
				st = subset
				syncer = New(api, st, &Options{SourceType: test.sourceType})
			}

			if test.chain {
				finalSource, err := st.GetOrCreateSource(test.sourceType, "final@example.test")
				require.NoError(err)
				require.NoError(st.AddAccountIdentity(finalSource.ID, "final@example.test", "manual"))
				api.labels = append(api.labels, &gmail.Label{ID: "Sent", Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent})
				_, err = syncer.syncLabels(t.Context(), finalSource.ID)
				require.NoError(err)
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: live.ID, IntoSourceID: finalSource.ID})
				require.NoError(err)
				live = finalSource
				api.labels[1].SystemRole = ""
			}

			nextID := test.nextName
			if test.rename {
				nextID = "Label_20"
				api.labels[1].Name = test.nextName
			} else if test.clearCoalesced {
				nextID = liveSentID
				api.labels[0].SystemRole = ""
			} else if !test.chain {
				api.labels = append(api.labels, &gmail.Label{ID: nextID, Name: test.nextName, Type: "system", SystemRole: test.nextRole})
			}
			nextLabels, err := syncer.syncLabels(t.Context(), live.ID)
			require.NoError(err)
			if test.rename {
				assert.Equal(liveLabels[nextID], nextLabels[nextID], "provider rename keeps its own row")
			}
			preserved := test.historyRole != "" && !test.coalesced
			if preserved {
				assert.NotEqual(archiveID, nextLabels[nextID], "provider must not adopt archive organization")
			}
			if test.repeatCollision {
				var archiveName string
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT name FROM labels WHERE id = ?`), archiveID).Scan(&archiveName))
				api.labels = append(api.labels, &gmail.Label{ID: archiveName, Name: archiveName, Type: "user"})
				nextLabels, err = syncer.syncLabels(t.Context(), live.ID)
				require.NoError(err)
				assert.NotEqual(archiveID, nextLabels[archiveName], "generated names are not the archive distinction")
			}
			labelIDs, err = st.MessageLabelIDsContext(t.Context(), id)
			require.NoError(err)
			assert.Equal([]int64{archiveID}, labelIDs, "historical membership keeps its meaning")
			var providerAfter, roleAfter sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_label_id, system_role FROM labels WHERE id = ?`), archiveID).Scan(&providerAfter, &roleAfter))
			if preserved {
				assert.False(providerAfter.Valid)
				assert.Equal(test.historyRole, roleAfter.String)
			}
			if test.historyRole == "" {
				assert.Equal(archiveID, nextLabels[nextID], "ordinary imported labels remain adoptable")
				assert.Equal(nextID, providerAfter.String)
			}
			var providerRole sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT system_role FROM labels WHERE id = ?`), nextLabels[nextID]).Scan(&providerRole))
			assert.Equal(test.nextRole, providerRole.String, "provider role remains authoritative on its row")
			results, _, err := st.SearchMessagesQuery(search.Parse("received:"+live.Identifier), 0, 100)
			require.NoError(err)
			if test.historyRole == "" || test.clearCoalesced {
				require.Len(results, 1)
				assert.Equal(id, results[0].ID)
			} else {
				assert.Empty(results)
				results, _, err = st.SearchMessagesQuery(search.Parse("account:history@example.test"), 0, 100)
				require.NoError(err)
				require.Len(results, 1)
				assert.Equal(id, results[0].ID)
			}
		})
	}
}
