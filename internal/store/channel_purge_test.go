package store_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPurgeChannelPreservesRepliesFromOtherChannels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(err)
	parent, err := st.EnsureConversation(source.ID, "channel-1", "Removed channel")
	require.NoError(err)
	retained, err := st.EnsureConversation(source.ID, "channel-2", "Retained channel")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: parent, SourceMessageID: "original", MessageType: "discord"})
	require.NoError(err)
	reply, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: retained, SourceMessageID: "reply", MessageType: "discord"})
	require.NoError(err)
	require.NoError(st.SetReplyTo(source.ID, "reply", "original"))

	require.NoError(st.PurgeChannelContext(t.Context(), source.ID, "channel-1"))
	var remaining int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM messages WHERE conversation_id = ?"), parent).Scan(&remaining))
	assert.Zero(remaining)
	var conversation int64
	var replyTo sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id, reply_to_message_id FROM messages WHERE id = ?"), reply).Scan(&conversation, &replyTo))
	assert.Equal(retained, conversation)
	assert.False(replyTo.Valid, "retained messages must no longer reference purged content")
}

func TestPurgeChannelRemovesOnlyUnsharedPackedMappings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "T01:U01")
	require.NoError(err)
	removed, err := st.EnsureConversation(source.ID, "C01", "Removed channel")
	require.NoError(err)
	retained, err := st.EnsureConversation(source.ID, "C02", "Retained channel")
	require.NoError(err)
	removedMsg, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: removed, SourceMessageID: "C01:1", MessageType: "slack"})
	require.NoError(err)
	retainedMsg, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: retained, SourceMessageID: "C02:1", MessageType: "slack"})
	require.NoError(err)
	unique, shared := strings.Repeat("a", 64), strings.Repeat("b", 64)
	require.NoError(st.UpsertAttachment(removedMsg, "unique.pdf", "application/pdf", unique[:2]+"/"+unique, unique, 10))
	require.NoError(st.UpsertAttachment(removedMsg, "shared.pdf", "application/pdf", shared[:2]+"/"+shared, shared, 10))
	require.NoError(st.UpsertAttachment(retainedMsg, "shared.pdf", "application/pdf", shared[:2]+"/"+shared, shared, 10))
	const packID = "01hzy3v7q8r9s0t1a2v3w4x5p1"
	require.NoError(st.RecordPackedBlobs(
		store.PackRecord{PackID: packID, EntryCount: 2, StoredBytes: 20, CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)},
		[]store.PackIndexEntry{
			{BlobHash: unique, PackID: packID, StoredLen: 10, RawLen: 10},
			{BlobHash: shared, PackID: packID, Offset: 10, StoredLen: 10, RawLen: 10},
		}))

	require.NoError(st.PurgeChannelContext(t.Context(), source.ID, "C01"))

	entry, err := st.GetAttachmentPackEntry(unique)
	require.NoError(err)
	assert.Nil(entry, "the purged channel's only reference removes its packed mapping")
	entry, err = st.GetAttachmentPackEntry(shared)
	require.NoError(err)
	assert.NotNil(entry, "a blob still referenced by a retained channel keeps its mapping")
}

func TestPurgeChannelToleratesMalformedSQLiteMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(err)
	_, err = st.EnsureConversation(source.ID, "channel-1", "Removed channel")
	require.NoError(err)
	thread, err := st.EnsureConversation(source.ID, "thread-1", "Removed thread")
	require.NoError(err)
	malformed, err := st.EnsureConversation(source.ID, "thread-2", "Unreadable thread")
	require.NoError(err)
	_, err = st.DB().Exec("UPDATE conversations SET metadata = ? WHERE id = ?", `{"parent_channel_id":"channel-1"}`, thread)
	require.NoError(err)
	_, err = st.DB().Exec("UPDATE conversations SET metadata = ? WHERE id = ?", "not json", malformed)
	require.NoError(err)

	require.NoError(st.PurgeChannelContext(t.Context(), source.ID, "channel-1"))

	var remaining int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM conversations WHERE source_id = ?", source.ID).Scan(&remaining))
	assert.Equal(1, remaining, "the parent and its thread are removed")
	var kept int64
	require.NoError(st.DB().QueryRow("SELECT id FROM conversations WHERE source_id = ?", source.ID).Scan(&kept))
	assert.Equal(malformed, kept, "the unreadable row stays")
}

// Analytics caches stamp the derived-data revision and rebuild when it moves,
// which is the only way they drop already exported rows.
func TestPurgesAdvanceDerivedDataRevision(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "T01:U01")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "C01", "Removed channel")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "C01:1", MessageType: "slack"})
	require.NoError(err)

	before, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	require.NoError(st.PurgeChannelContext(t.Context(), source.ID, "C01"))
	afterChannel, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(t, before+1, afterChannel, "channel purge")

	_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
	require.NoError(err)
	afterSource, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(t, afterChannel+1, afterSource, "source removal")
}

func TestPurgeChannelRefusesQueuedSyncOperation(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "T01:U01")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "C01", "Selected channel")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "C01:1", MessageType: "slack"})
	require.NoError(err)
	_, err = st.CreateSyncOperation(source.ID, "pending-operation")
	require.NoError(err)

	require.ErrorIs(st.PurgeChannelContext(t.Context(), source.ID, "C01"), store.ErrSyncAlreadyActive)
	var remaining int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM messages WHERE conversation_id = ?"), conversation).Scan(&remaining))
	assert.Equal(t, 1, remaining, "a refused purge removes nothing")
}
