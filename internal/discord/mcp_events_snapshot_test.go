package discord

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func enableNativeDiscordEvents(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "discord", Kinds: []string{"message"}}}})
	require.NoError(t, err)
}

// Exercise the native page writer under a live ingest context. Real database
// triggers reject mandatory writes; the external API is unused at this boundary.
func TestMCPDiscordSnapshotRollback(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "message_recipients", "conversation_participants", "attachments"} {
		t.Run(table, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			enableNativeDiscordEvents(t, st)
			source, err := st.GetOrCreateSource("discord", "200")
			require.NoError(err)
			conv, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Synthetic channel")
			require.NoError(err)
			imp := newTestImporter(st.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now()}), nil)
			message := importerTestMessage("501", "300", "Synthetic ready message")
			message.Attachments = []Attachment{{ID: "601", Filename: "synthetic.txt", ContentType: "text/plain", Size: 4}}
			release := failNativeDiscordInsert(t, st, table)
			summary := &ImportSummary{processedMessageIDs: map[string]struct{}{}}
			err = imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil)
			require.Error(err)
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Zero(count, "mandatory snapshot failure must roll back the message")
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count, "an incomplete snapshot must never publish an occurrence")
			release()
			require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Equal(1, count)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Equal(1, count, "retry retains first-arrival eligibility")
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&count))
			assert.Equal(1, count)
		})
	}
}

func failNativeDiscordInsert(t *testing.T, st *store.Store, table string) func() {
	t.Helper()
	require := require.New(t)
	name := "fail_discord_snapshot_" + table
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic snapshot failure'; END; $$`, name))
		require.NoError(err)
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, table, name))
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'synthetic snapshot failure'); END`, name, table))
		require.NoError(err)
	}
	release := func() {
		if st.IsPostgreSQL() {
			_, err := st.DB().Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
			require.NoError(err)
			_, err = st.DB().Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
			require.NoError(err)
		} else {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS ` + name)
			require.NoError(err)
		}
	}
	t.Cleanup(release)
	return release
}

func TestMCPDiscordReferenceIdentity(t *testing.T) {
	for _, tc := range []struct{ name, guild, channel string }{
		{"foreign guild", "201", "300"},
		{"different channel", "200", "399"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("discord", "200")
			require.NoError(err)
			conv, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Synthetic channel")
			require.NoError(err)
			imp := newTestImporter(st, nil)
			parent := importerTestMessage("501", "300", "Synthetic parent")
			child := importerTestMessage("502", "300", "Synthetic child")
			child.MessageReference = &MessageReference{MessageID: parent.ID, GuildID: tc.guild, ChannelID: tc.channel}
			summary := &ImportSummary{processedMessageIDs: map[string]struct{}{}}
			require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{parent, child}, summary, nil))
			var linked sql.NullInt64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE source_message_id=?`), child.ID).Scan(&linked))
			assert.False(linked.Valid, "a reference identity mismatch must remain metadata only")
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET reply_to_message_id=NULL WHERE source_message_id=?`), child.ID)
			require.NoError(err)
			require.NoError(imp.resolveDeferredReplies(source.ID))
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE source_message_id=?`), child.ID).Scan(&linked))
			assert.False(linked.Valid, "deferred repair must enforce the same native identity")
		})
	}
}

func TestMCPDiscordUnavailableParentRetainsArrival(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Synthetic channel")
	require.NoError(err)
	imp := newTestImporter(st.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now()}), nil)
	child := importerTestMessage("502", "300", "Synthetic child with unavailable parent")
	child.MessageReference = &MessageReference{MessageID: "501", ChannelID: "300", GuildID: "200"}
	summary := &ImportSummary{processedMessageIDs: map[string]struct{}{}}
	require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{child}, summary, nil))
	var linked sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE source_message_id=?`), child.ID).Scan(&linked))
	assert.False(linked.Valid)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "an optional unavailable parent must not suppress the live child")
	historical := newTestImporter(st.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill}), nil)
	parent := importerTestMessage("501", "300", "Synthetic recovered parent")
	require.NoError(historical.persistPage(t.Context(), source.ID, conv, []Message{parent}, summary, nil))
	require.NoError(imp.resolveDeferredReplies(source.ID))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE source_message_id=?`), child.ID).Scan(&linked))
	assert.True(linked.Valid)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "later parent recovery and link repair must stay silent")
	require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{child}, summary, nil))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count)
}

func TestMCPDiscordSnapshotRetainsDownloadedAliases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Synthetic channel")
	require.NoError(err)
	imp := newTestImporter(st, nil)
	message := importerTestMessage("501", "300", "Synthetic attachment snapshot")
	message.Attachments = []Attachment{
		{ID: "601", Filename: "first.txt", Size: 4},
		{ID: "602", Filename: "second.txt", Size: 4},
	}
	summary := &ImportSummary{processedMessageIDs: map[string]struct{}{}}
	require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil))
	id := messageIDBySource(t, st, source.ID, message.ID)
	hash := strings.Repeat("a", 64)
	path := "aa/" + hash
	require.NoError(st.ReplaceMessageDiscordAttachments(id, []store.AttachmentRef{
		{SourceAttachmentID: "discord:601", Filename: "first.txt", StoragePath: path, ContentHash: hash, Size: 8},
		{SourceAttachmentID: "discord:602", Filename: "second.txt", StoragePath: path, Size: 8},
	}))
	// Seed the supported legacy hashless representation; the public reader
	// intentionally resolves its canonical hash from the trusted CAS path.
	_, err = st.DB().Exec(st.Rebind(`UPDATE attachments SET content_hash=NULL WHERE message_id=? AND source_attachment_id=?`), id, "discord:602")
	require.NoError(err)
	require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil))
	refs, err := st.MessageDiscordAttachments(id)
	require.NoError(err)
	require.Len(refs, 2)
	for _, key := range []string{"discord:601", "discord:602"} {
		assert.True(store.IsDiscordAttachmentDownloaded(refs[key]), "snapshot must preserve each trusted CAS reference")
		assert.Equal(path, refs[key].StoragePath)
		assert.Equal(8, refs[key].Size)
	}
	assert.Equal(hash, refs["discord:602"].ContentHash, "reader resolves a legacy alias to its canonical hash")
	var storedHash sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT content_hash FROM attachments WHERE message_id=? AND source_attachment_id=?`), id, "discord:602").Scan(&storedHash))
	assert.Equal(sql.NullString{String: hash, Valid: true}, storedHash, "rewriting a legacy alias must retain canonical database evidence")
	message.Attachments = nil
	require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil))
	refs, err = st.MessageDiscordAttachments(id)
	require.NoError(err)
	assert.Empty(refs, "an authoritative empty snapshot removes old provider references")
}

func TestMCPDiscordExistingSnapshotRollback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Synthetic channel")
	require.NoError(err)
	imp := newTestImporter(st.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now()}), nil)
	message := importerTestMessage("501", "300", "Synthetic original body")
	summary := &ImportSummary{processedMessageIDs: map[string]struct{}{}}
	require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil))
	id := messageIDBySource(t, st, source.ID, message.ID)
	message.Content = "Synthetic changed body"
	message.Attachments = []Attachment{{ID: "601", Filename: "synthetic.txt", Size: 4}}
	release := failNativeDiscordInsert(t, st, "attachments")
	require.Error(imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil))
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body))
	assert.Equal("Synthetic original body", body)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count)
	release()
	require.NoError(imp.persistPage(t.Context(), source.ID, conv, []Message{message}, summary, nil))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body))
	assert.Equal("Synthetic changed body", body)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&count))
	assert.Equal(1, count)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "an update never becomes another arrival")
}
