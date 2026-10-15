package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const previousSQLiteAttachmentChangeUpdateTrigger = `CREATE TRIGGER trg_attachment_change_update
		    AFTER UPDATE OF message_id, filename, mime_type, size, content_hash,
		        storage_path, media_type, width, height, duration_ms,
		        source_attachment_id, attachment_metadata, attachment_role,
		        role_source, source_part_key, content_id, encryption_version
		    ON attachments FOR EACH ROW
		    WHEN EXISTS (SELECT 1 FROM attachment_change_consumers)
		      AND (OLD.message_id IS NOT NEW.message_id
		        OR OLD.filename IS NOT NEW.filename
		        OR OLD.mime_type IS NOT NEW.mime_type
		        OR OLD.size IS NOT NEW.size
		        OR OLD.content_hash IS NOT NEW.content_hash
		        OR OLD.storage_path IS NOT NEW.storage_path
		        OR OLD.media_type IS NOT NEW.media_type
		        OR OLD.width IS NOT NEW.width
		        OR OLD.height IS NOT NEW.height
		        OR OLD.duration_ms IS NOT NEW.duration_ms
		        OR OLD.source_attachment_id IS NOT NEW.source_attachment_id
		        OR OLD.attachment_metadata IS NOT NEW.attachment_metadata
		        OR OLD.attachment_role IS NOT NEW.attachment_role
		        OR OLD.role_source IS NOT NEW.role_source
		        OR OLD.source_part_key IS NOT NEW.source_part_key
		        OR OLD.content_id IS NOT NEW.content_id
		        OR OLD.encryption_version IS NOT NEW.encryption_version)
		    BEGIN
		        INSERT INTO attachment_change_log
		            (event_kind, old_message_id, new_message_id,
		             old_attachment_id, new_attachment_id,
		             old_content_hash, new_content_hash,
		             old_source_part_key, new_source_part_key,
		             old_role, new_role)
		        VALUES ('attachment_update', OLD.message_id, NEW.message_id,
		                OLD.id, NEW.id, OLD.content_hash, NEW.content_hash,
		                OLD.source_part_key, NEW.source_part_key,
		                OLD.attachment_role, NEW.attachment_role);
		    END`

const previousPostgresAttachmentChangeUpdateTrigger = `CREATE TRIGGER trg_attachment_change_update
		     AFTER UPDATE OF message_id, filename, mime_type, size, content_hash,
		         storage_path, media_type, width, height, duration_ms,
		         source_attachment_id, attachment_metadata, attachment_role,
		         role_source, source_part_key, content_id, encryption_version
		     ON attachments FOR EACH ROW
		     WHEN (OLD.message_id IS DISTINCT FROM NEW.message_id
		        OR OLD.filename IS DISTINCT FROM NEW.filename
		        OR OLD.mime_type IS DISTINCT FROM NEW.mime_type
		        OR OLD.size IS DISTINCT FROM NEW.size
		        OR OLD.content_hash IS DISTINCT FROM NEW.content_hash
		        OR OLD.storage_path IS DISTINCT FROM NEW.storage_path
		        OR OLD.media_type IS DISTINCT FROM NEW.media_type
		        OR OLD.width IS DISTINCT FROM NEW.width
		        OR OLD.height IS DISTINCT FROM NEW.height
		        OR OLD.duration_ms IS DISTINCT FROM NEW.duration_ms
		        OR OLD.source_attachment_id IS DISTINCT FROM NEW.source_attachment_id
		        OR OLD.attachment_metadata IS DISTINCT FROM NEW.attachment_metadata
		        OR OLD.attachment_role IS DISTINCT FROM NEW.attachment_role
		        OR OLD.role_source IS DISTINCT FROM NEW.role_source
		        OR OLD.source_part_key IS DISTINCT FROM NEW.source_part_key
		        OR OLD.content_id IS DISTINCT FROM NEW.content_id
		        OR OLD.encryption_version IS DISTINCT FROM NEW.encryption_version)
		     EXECUTE FUNCTION capture_attachment_change()`

func TestInitSchemaUpgradesAttachmentStateChangeTrigger(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("attachment-state-upgrade")
	attachmentID := createJournalAttachment(t, f, messageID, "part:1", "a")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET attachment_state = 'pending' WHERE id = ?`), attachmentID)
	require.NoError(err)

	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "document-index/v1")
	require.NoError(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(t.Context(), consumer.ConsumerKey, consumer.BaselineSequence))

	drop := "DROP TRIGGER trg_attachment_change_update"
	previousTrigger := previousSQLiteAttachmentChangeUpdateTrigger
	if f.Store.IsPostgreSQL() {
		drop += " ON attachments"
		previousTrigger = previousPostgresAttachmentChangeUpdateTrigger
	}
	_, err = f.Store.DB().Exec(drop)
	require.NoError(err)
	_, err = f.Store.DB().Exec(previousTrigger)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`DELETE FROM applied_migrations WHERE name = 'message_and_attachment_triggers_v12'`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`UPDATE archive_metadata SET value = '9' WHERE key = 'schema_version'`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`INSERT INTO applied_migrations (name) VALUES ('message_and_attachment_triggers_v10') ON CONFLICT (name) DO NOTHING`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`INSERT INTO applied_migrations (name) VALUES ('message_and_attachment_triggers_v11') ON CONFLICT (name) DO NOTHING`)
	require.NoError(err)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET attachment_state = 'stored' WHERE id = ?`), attachmentID)
	require.NoError(err)
	assert.Zero(attachmentChangeCount(t, f), "the previous trigger misses state-only changes")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET attachment_state = 'pending' WHERE id = ?`), attachmentID)
	require.NoError(err)

	require.NoError(f.Store.InitSchema())
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET attachment_state = 'stored' WHERE id = ?`), attachmentID)
	require.NoError(err)
	changes, err := f.Store.ListAttachmentChanges(t.Context(), consumer.ConsumerKey, 10)
	require.NoError(err)
	require.Len(changes, 1)
	assert.Equal("attachment_update", changes[0].EventKind)
	assert.Equal(attachmentID, requireInt64Pointer(t, changes[0].OldAttachmentID))
	assert.Equal(attachmentID, requireInt64Pointer(t, changes[0].NewAttachmentID))
	assert.Equal(messageID, requireInt64Pointer(t, changes[0].NewMessageID))

	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET attachment_state = 'stored' WHERE id = ?`), attachmentID)
	require.NoError(err)
	assert.Equal(1, attachmentChangeCount(t, f), "an unchanged state emits no extra event")
}
