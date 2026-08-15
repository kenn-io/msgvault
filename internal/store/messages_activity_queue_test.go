package store_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestMessagesActivityColumnsAreRealColumns keeps the trigger's hand-written
// column list honest against the live table on both backends: a typo or a
// renamed column would otherwise fail only when the trigger is next rebuilt on
// an upgrading archive.
func TestMessagesActivityColumnsAreRealColumns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	actual, err := store.MessagesTableColumns(st)
	require.NoError(err)
	actualSet := map[string]bool{}
	for _, col := range actual {
		actualSet[col] = true
	}
	require.NotEmpty(store.MessagesActivityColumns)
	for _, col := range store.MessagesActivityColumns {
		assert.True(actualSet[col],
			"MessagesActivityColumns names %q, which is not a column of messages", col)
	}
}

// TestMessageBookkeepingUpdatesDoNotRequeueActivity pins the scoped messages
// trigger: embedding, attachment-counter, and idempotent re-sync writes touch
// every message in the archive and none of them changes an activity input, so
// none may requeue the message. A real activity input change still must.
func TestMessageBookkeepingUpdatesDoNotRequeueActivity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	builder := f.NewMessage().WithSourceMessageID("bookkeeping-message")
	messageID := builder.Create(t, f.Store)
	before := activityQueueRevision(t, f.Store, messageID)

	require.NoError(f.Store.SetEmbedGen(t.Context(), []int64{messageID}, 7))
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"an embed_gen stamp must not requeue the message")

	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET has_attachments = TRUE, attachment_count = 1 WHERE id = ?`),
		messageID)
	require.NoError(err)
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"the ingest attachment-counter re-stamp must not requeue the message")

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = ?`),
		messageID)
	require.NoError(err)
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"a last_modified bump must not requeue the message")

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET message_type = message_type, sent_at = sent_at,
		        deleted_at = deleted_at WHERE id = ?`),
		messageID)
	require.NoError(err)
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"naming an activity column without changing its value must not requeue")

	sameID, err := f.Store.UpsertMessage(builder.Build())
	require.NoError(err)
	require.Equal(messageID, sameID)
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"an idempotent re-sync of a known message must not requeue it")

	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "bookkeeping-message"))
	assert.Greater(activityQueueRevision(t, f.Store, messageID), before,
		"a deleted_from_source_at change retracts the event and must requeue")
}

// TestActivityMessagesTriggerUpgradeReplacesBlanketDefinition proves the
// production upgrade path replaces the blanket trigger an earlier build left on
// a SQLite archive. schema.sql cannot do it: CREATE TRIGGER IF NOT EXISTS keeps
// whatever definition is already there, so the swap has to ride the
// EnsureActivityProjectionTriggers migration.
func TestActivityMessagesTriggerUpgradeReplacesBlanketDefinition(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL always DROP + CREATEs its activity triggers")
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "blanket-activity.db")

	seed, err := store.OpenForTest(dbPath)
	require.NoError(err, "open seed store")
	require.NoError(seed.InitSchema(), "seed InitSchema")
	_, err = seed.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'alice@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject)
VALUES (1, 1, 1, 'm1', 'email', 'seeded');
`)
	require.NoError(err, "seed rows")
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS trg_activity_queue_messages_update`,
		// Verbatim from schema.sql before the trigger moved to
		// EnsureActivityProjectionTriggers.
		`CREATE TRIGGER trg_activity_queue_messages_update
		 AFTER UPDATE ON messages FOR EACH ROW
		 BEGIN
		     INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		     VALUES (NEW.id, 1, CURRENT_TIMESTAMP)
		     ON CONFLICT(message_id) DO UPDATE SET
		         revision = activity_projection_queue.revision + 1,
		         queued_at = CURRENT_TIMESTAMP;
		 END`,
		`DELETE FROM applied_migrations WHERE name = 'activity_projection_triggers_v4'`,
	} {
		_, err = seed.DB().Exec(stmt)
		require.NoErrorf(err, "wind the archive back: %s", stmt)
	}
	require.NoError(seed.Close(), "close seed store")

	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "reopen the archive")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "InitSchema on an archive with the blanket trigger")

	var postSQL string
	require.NoError(st.DB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'trigger'
		   AND name = 'trg_activity_queue_messages_update'`).Scan(&postSQL))
	assert.Contains(postSQL, "AFTER UPDATE OF",
		"the blanket definition must be replaced by the scoped one on open")

	_, err = st.DB().Exec(`UPDATE messages SET embed_gen = 1 WHERE id = 1`)
	require.NoError(err)
	assert.Equal(0, activityQueueRows(t, st, 1),
		"an embed_gen stamp must not enqueue on the upgraded archive")

	_, err = st.DB().Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = 1`)
	require.NoError(err)
	assert.Equal(1, activityQueueRows(t, st, 1),
		"a deleted_at change must still enqueue on the upgraded archive")
}
