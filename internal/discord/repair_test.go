package discord

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDiscordRederiveRegistration(t *testing.T) {
	assert := assert.New(t)
	fn, version, ok := rederive.Lookup(sourceTypeDiscord)
	assert.True(ok)
	assert.NotNil(fn)
	assert.Equal(discordRederiveVersion, version)
}

func TestDiscordRepairAddsVoiceMetadataAndPreservesStoredMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeDiscord, "repair-source")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "repair-channel", "channel", "repair")
	require.NoError(err)

	voiceID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "voice-message",
		MessageType: discordMessageType,
		SentAt:      sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	voiceRaw := []byte(`{"id":"voice-message","channel_id":"repair-channel","type":0,"flags":8192,"content":"voice","attachments":[{"id":"voice-1","filename":"empty.ogg","content_type":"audio/ogg","size":12,"duration_secs":5.94,"waveform":""},{"id":"voice-2","filename":"voice.ogg","content_type":"audio/ogg","size":18,"duration_secs":1.25,"waveform":"%%%"}]}`)
	require.NoError(st.UpsertMessageRawWithFormat(voiceID, voiceRaw, discordRawFormat))
	hash := "abababababababababababababababababababababababababababababababab"
	require.NoError(st.ReplaceMessageDiscordAttachments(voiceID, []store.AttachmentRef{
		{Filename: "empty.ogg", MimeType: "audio/ogg", StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Size: 12, SourceAttachmentID: "discord:voice-1"},
		{Filename: "voice.ogg", MimeType: "audio/ogg", StoragePath: "discord:pending:voice-2", Size: 18, SourceAttachmentID: "discord:voice-2"},
		{Filename: "stale.ogg", StoragePath: "discord:pending:stale", SourceAttachmentID: "discord:stale"},
	}))
	_, err = st.SetDiscordAttachmentMetadata(voiceID, map[string]string{
		"discord:voice-1": `{"old":1}`,
		"discord:voice-2": `{"old":2}`,
		"discord:stale":   `{"old":3}`,
	})
	require.NoError(err)
	before, err := st.MessageDiscordAttachments(voiceID)
	require.NoError(err)
	rawBefore, err := st.GetMessageRaw(voiceID)
	require.NoError(err)

	invalidID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "invalid-message",
		MessageType: discordMessageType,
		SentAt:      sql.NullTime{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	invalidRaw := []byte(`{"id":"invalid-message","flags":8192`)
	require.NoError(st.UpsertMessageRawWithFormat(invalidID, invalidRaw, discordRawFormat))
	require.NoError(st.ReplaceMessageDiscordAttachments(invalidID, []store.AttachmentRef{{
		SourceAttachmentID: "discord:invalid", StoragePath: "discord:pending:invalid",
	}}))
	_, err = st.SetDiscordAttachmentMetadata(invalidID, map[string]string{"discord:invalid": `{"old":4}`})
	require.NoError(err)

	emptyID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "empty-message",
		MessageType: discordMessageType,
		SentAt:      sql.NullTime{Time: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	emptyRaw := []byte(`{"id":"empty-message","channel_id":"repair-channel","type":0,"flags":0,"attachments":[]}`)
	require.NoError(st.UpsertMessageRawWithFormat(emptyID, emptyRaw, discordRawFormat))
	require.NoError(st.ReplaceMessageDiscordAttachments(emptyID, []store.AttachmentRef{{
		SourceAttachmentID: "discord:stale-empty", StoragePath: "discord:pending:stale-empty",
	}}))
	_, err = st.SetDiscordAttachmentMetadata(emptyID, map[string]string{"discord:stale-empty": `{"old":5}`})
	require.NoError(err)

	sum, err := NewImporter(st, nil).RepairSource(t.Context(), source.ID, nil)
	require.NoError(err)
	assert.Equal(int64(3), sum.MessagesScanned)
	assert.Equal(int64(2), sum.MessageMetadataRewritten)
	assert.Equal(int64(1), sum.Undecodable)
	assert.Equal(int64(0), sum.Errors)
	assert.Equal(int64(4), sum.AttachmentsTagged)
	metadata, err := st.GetMessageMetadata(voiceID)
	require.NoError(err)
	assert.JSONEq(`{"discord_message_type":0,"discord_message_flags":8192}`, metadata.String)
	refs, err := st.MessageDiscordAttachments(voiceID)
	require.NoError(err)
	assert.JSONEq(`{"discord":{}}`, refs["discord:voice-1"].Metadata)
	assert.JSONEq(`{"discord":{"waveform":"%%%"}}`, refs["discord:voice-2"].Metadata)
	assert.Empty(refs["discord:stale"].Metadata)
	assert.Equal(before["discord:voice-1"].StoragePath, refs["discord:voice-1"].StoragePath)
	assert.Equal(before["discord:voice-1"].ContentHash, refs["discord:voice-1"].ContentHash)
	assert.Equal(before["discord:voice-1"].Size, refs["discord:voice-1"].Size)
	assert.Equal(before["discord:voice-1"].State, refs["discord:voice-1"].State)
	assert.Equal(before["discord:voice-1"].Role, refs["discord:voice-1"].Role)
	assert.Equal(before["discord:voice-1"].RoleSource, refs["discord:voice-1"].RoleSource)
	assert.Equal(rawBefore, mustRaw(t, st, voiceID))
	emptyRefs, err := st.MessageDiscordAttachments(emptyID)
	require.NoError(err)
	assert.Empty(emptyRefs["discord:stale-empty"].Metadata)
	invalidRefs, err := st.MessageDiscordAttachments(invalidID)
	require.NoError(err)
	assert.JSONEq(`{"old":4}`, invalidRefs["discord:invalid"].Metadata)

	second, err := NewImporter(st, nil).RepairSource(t.Context(), source.ID, nil)
	require.NoError(err)
	assert.Equal(int64(0), second.AttachmentsTagged)
	assert.Equal(int64(0), second.MessageMetadataRewritten)
	assert.Equal(int64(0), second.Errors)
}

func mustRaw(t *testing.T, st *store.Store, messageID int64) []byte {
	t.Helper()
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	return raw
}
