package discord

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMergeSourcesRestartsDiscordRepairForMovedHistory(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		completedDestination, repairedHistory bool
	}{
		{name: "completed destination", completedDestination: true},
		{name: "fresh destination repairs moved history"},
		{name: "completed destination with already repaired history", completedDestination: true, repairedHistory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			live, err := st.GetOrCreateSource(sourceTypeDiscord, "200")
			require.NoError(err)
			history, err := st.GetOrCreateSource(sourceTypeDiscord, "201")
			require.NoError(err)
			liveConversation, err := st.EnsureConversationWithType(live.ID, "300", "channel", "Live")
			require.NoError(err)
			historyConversation, err := st.EnsureConversationWithType(history.ID, "301", "channel", "History")
			require.NoError(err)
			liveID, err := st.UpsertMessage(&store.Message{SourceID: live.ID, ConversationID: liveConversation, SourceMessageID: "502", MessageType: discordMessageType})
			require.NoError(err)
			require.NoError(st.UpsertMessageRawWithFormat(liveID, []byte(`{"id":"502","channel_id":"300","type":0,"flags":0,"content":"current archive"}`), discordRawFormat))
			if tc.completedDestination {
				sum, ran, err := rederive.RunIfStale(t.Context(), st, sourceTypeDiscord, live.Identifier, live.ID, nil)
				require.NoError(err)
				require.True(ran)
				require.Equal(int64(1), sum.MessagesScanned)
				require.Equal(int64(1), sum.MessageMetadataRewritten)
				require.Zero(sum.Errors)
			}

			historicalID, err := st.UpsertMessage(&store.Message{SourceID: history.ID, ConversationID: historyConversation, SourceMessageID: "501", MessageType: discordMessageType})
			require.NoError(err)
			const historicalRaw = `{"id":"501","channel_id":"301","type":0,"flags":8192,"content":"archived voice","attachments":[{"id":"401","filename":"voice.ogg","content_type":"audio/ogg","size":12,"duration_secs":5.94,"waveform":"%%%"}]}`
			require.NoError(st.UpsertMessageRawWithFormat(historicalID, []byte(historicalRaw), discordRawFormat))
			require.NoError(st.SetMessageMetadataContext(t.Context(), historicalID, sql.NullString{String: `{"discord_message_type":0}`, Valid: true}))
			const hash = "abababababababababababababababababababababababababababababababab"
			require.NoError(st.ReplaceMessageDiscordAttachments(historicalID, []store.AttachmentRef{{Filename: "voice.ogg", MimeType: "audio/ogg", StoragePath: "ab/" + hash, ContentHash: hash, Size: 12, SourceAttachmentID: "discord:401"}}))
			if tc.repairedHistory {
				sum, err := rederive.Run(t.Context(), st, sourceTypeDiscord, history.Identifier, history.ID, nil)
				require.NoError(err)
				require.Equal(int64(1), sum.MessageMetadataRewritten)
				require.Equal(int64(1), sum.AttachmentsTagged)
			}
			before, err := st.MessageDiscordAttachments(historicalID)
			require.NoError(err)
			require.Contains(before, "discord:401")
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID, DryRun: true})
			require.NoError(err)
			applied, err := st.IsMigrationAppliedContext(t.Context(), rederive.LedgerKey(sourceTypeDiscord, live.Identifier, discordRederiveVersion), 1)
			require.NoError(err)
			assert.Equal(tc.completedDestination, applied, "preview preserves completed repairs")

			merged, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
			require.NoError(err)
			require.Equal(int64(1), merged.MessagesMoved)
			require.Zero(merged.DuplicatesHidden)
			var currentSource int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_id FROM messages WHERE id = ?`), historicalID).Scan(&currentSource))
			require.Equal(live.ID, currentSource)
			// This is the real automatic entry point. The provider exposes only
			// the live channel, so moved historical raw data must repair locally.
			sync, err := newTestImporter(st, newImporterFakeAPI(importerTestChannel("300", "Live"))).Import(t.Context(), ImportOptions{GuildID: live.Identifier, Full: true})
			require.NoError(err)
			assert.True(sync.RepairRan, "moved history invalidates the completed destination repair")
			if tc.repairedHistory {
				assert.Zero(sync.MessageMetadataRepaired)
				assert.Zero(sync.AttachmentsRetagged)
			} else {
				wantRepaired := int64(1)
				if !tc.completedDestination {
					wantRepaired = 2
				}
				assert.Equal(wantRepaired, sync.MessageMetadataRepaired)
				assert.Equal(int64(1), sync.AttachmentsRetagged)
			}
			assert.Zero(sync.RepairErrors)
			automaticMetadata, err := st.GetMessageMetadata(historicalID)
			require.NoError(err)
			assert.JSONEq(`{"discord_message_type":0,"discord_message_flags":8192}`, automaticMetadata.String)
			automaticAttachments, err := st.MessageDiscordAttachments(historicalID)
			require.NoError(err)
			assert.JSONEq(`{"discord":{"waveform":"%%%"}}`, automaticAttachments["discord:401"].Metadata)

			full, err := rederive.Run(t.Context(), st, sourceTypeDiscord, live.Identifier, live.ID, nil)
			require.NoError(err)
			assert.Equal(int64(2), full.MessagesScanned)
			assert.Zero(full.Errors)
			assert.Zero(full.MessageMetadataRewritten, "automatic repair covers the same history as a full pass")
			assert.Zero(full.AttachmentsTagged)

			afterFull, err := st.GetMessageMetadata(historicalID)
			require.NoError(err)
			assert.JSONEq(`{"discord_message_type":0,"discord_message_flags":8192}`, afterFull.String)
			attachments, err := st.MessageDiscordAttachments(historicalID)
			require.NoError(err)
			assert.JSONEq(`{"discord":{"waveform":"%%%"}}`, attachments["discord:401"].Metadata)
			assert.Equal(before["discord:401"].ContentHash, attachments["discord:401"].ContentHash)
			assert.Equal(before["discord:401"].StoragePath, attachments["discord:401"].StoragePath)
			raw, err := st.GetMessageRaw(historicalID)
			require.NoError(err)
			assert.Equal([]byte(historicalRaw), raw) //nolint:testifylint // Preserve the exact archived provider bytes, including their encoding.
			merged, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
			require.NoError(err)
			assert.True(merged.AlreadyMerged)
			_, ran, err := rederive.RunIfStale(t.Context(), st, sourceTypeDiscord, live.Identifier, live.ID, nil)
			require.NoError(err)
			assert.False(ran, "retrying a committed merge preserves completed repair")
		})
	}
}
