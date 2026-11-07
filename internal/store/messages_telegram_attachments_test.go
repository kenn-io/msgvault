package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersistMessageAttachmentSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user1")
	require.NoError(err)
	attachments := []store.AttachmentWrite{{Filename: "photo.jpg", ContentHash: "example-photo", SourcePartKey: "photo", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}}
	data := &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "user2:1", MessageType: "telegram"}, Conversation: &store.ConversationPersistData{SourceConversationID: "user2", ConversationType: "direct_chat"}, MIMEAttachmentReplacement: &attachments, Edited: true}
	var n int
	id, err := st.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT attachment_count FROM messages WHERE id = ?"), id).Scan(&n))
	assert.Equal(1, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM attachments WHERE message_id = ?"), id).Scan(&n))
	assert.Equal(1, n)
	var edited bool
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT is_edited FROM messages WHERE id = ?"), id).Scan(&edited))
	assert.True(edited)
	// A rejected attachment must roll back the message transaction too.
	data.Message.SourceMessageID = "user2:2"
	attachments[0].Role = "invalid"
	_, err = st.PersistMessageContext(t.Context(), data)
	require.Error(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM messages WHERE source_message_id = ?"), "user2:2").Scan(&n))
	assert.Zero(n)
	data.Message.SourceMessageID = "user2:3"
	attachments[0].Role = store.AttachmentRoleStandalone
	// A failed edit marker must roll back the message and its attachments.
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_edit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'edit interrupted'; END $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_edit BEFORE UPDATE OF is_edited ON messages FOR EACH ROW EXECUTE FUNCTION fail_edit()`)
		require.NoError(err)
	} else {
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_edit BEFORE UPDATE OF is_edited ON messages BEGIN SELECT RAISE(ABORT, 'edit interrupted'); END`)
		require.NoError(err)
	}
	_, err = st.PersistMessageContext(t.Context(), data)
	require.ErrorContains(err, "edit interrupted")
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM messages WHERE source_message_id = ?"), "user2:3").Scan(&n))
	assert.Zero(n)
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_edit ON messages`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_edit`)
	}
	require.NoError(err)
	id, err = st.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT is_edited FROM messages WHERE id = ?"), id).Scan(&edited))
	assert.True(edited)
}

func TestImportMediaRecoveryRollsBackWithStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user1")
	require.NoError(err)
	attachments := []store.AttachmentWrite{{Filename: "photo.jpg", SourcePartKey: "photo", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}}
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "user2:1", MessageType: "telegram"}, Conversation: &store.ConversationPersistData{SourceConversationID: "user2", ConversationType: "direct_chat"}, MIMEAttachmentReplacement: &attachments})
	require.NoError(err)
	write := attachments[0]
	write.ContentHash = "example-photo"
	write.StoragePath = "photo.jpg"
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_media_stats() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'stats unavailable'; END $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_media_stats BEFORE UPDATE OF attachment_count ON messages FOR EACH ROW EXECUTE FUNCTION fail_media_stats()`)
		require.NoError(err)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_media_stats BEFORE UPDATE OF attachment_count ON messages BEGIN SELECT RAISE(ABORT, 'stats unavailable'); END`)
		require.NoError(err)
	}
	err = st.UpsertAttachmentRecordWithStats(t.Context(), id, write, true)
	require.ErrorContains(err, "stats unavailable")
	var path string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT storage_path FROM attachments").Scan(&path))
	assert.Empty(path)
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_media_stats ON messages`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_media_stats`)
	}
	require.NoError(err)
	require.NoError(st.UpsertAttachmentRecordWithStats(t.Context(), id, write, true))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT storage_path FROM attachments").Scan(&path))
	assert.NotEmpty(path)
}
