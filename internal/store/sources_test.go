package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestStore_GetSourcesByIdentifier(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	// Create two sources with same identifier, different types
	_, err := st.GetOrCreateSource("gmail", "user@example.com")
	require.NoError(err, "create gmail source")
	_, err = st.GetOrCreateSource("mbox", "user@example.com")
	require.NoError(err, "create mbox source")

	sources, err := st.GetSourcesByIdentifier("user@example.com")
	require.NoError(err, "GetSourcesByIdentifier")
	require.Len(sources, 2)

	// Verify ordering by source_type
	assert.Equal("gmail", sources[0].SourceType, "sources[0].SourceType")
	assert.Equal("mbox", sources[1].SourceType, "sources[1].SourceType")
}

func TestStore_GetSourcesByIdentifier_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	sources, err := st.GetSourcesByIdentifier("nobody@example.com")
	requirepkg.NoError(t, err, "GetSourcesByIdentifier")
	assertpkg.Empty(t, sources)
}

func TestStore_RemoveSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// Create messages, labels, and FTS data
	msgID := f.CreateMessage("msg-remove-1")
	f.CreateMessage("msg-remove-2")

	labels := f.EnsureLabels(map[string]string{
		"INBOX": "Inbox",
	}, "system")
	err := f.Store.ReplaceMessageLabels(msgID, []int64{labels["INBOX"]})
	require.NoError(err, "ReplaceMessageLabels")

	if f.Store.FTS5Available() {
		err = f.Store.UpsertFTS(msgID, "Test", "body", "a@b.com", "", "")
		require.NoError(err, "UpsertFTS")
	}

	// Remove source
	err = f.Store.RemoveSource(f.Source.ID)
	require.NoError(err, "RemoveSource")

	// Verify source is gone
	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "source should be nil after removal")

	// Verify messages are gone
	count, err := f.Store.CountMessagesForSource(f.Source.ID)
	require.NoError(err, "CountMessagesForSource")
	assert.Equal(int64(0), count, "message count")

	// Verify labels are gone
	var labelCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM labels WHERE source_id = ?`), f.Source.ID,
	).Scan(&labelCount)
	require.NoError(err, "count labels")
	assert.Equal(0, labelCount, "label count")

	// Verify FTS rows are gone (SQLite FTS5 vtable only; on PG the
	// equivalent invariant — search_fts cleared — is covered by the
	// dialect-level FTSDeleteSQL test).
	if f.Store.FTS5Available() && !f.Store.IsPostgreSQL() {
		var ftsCount int
		err = f.Store.DB().QueryRow(
			`SELECT COUNT(*) FROM messages_fts`,
		).Scan(&ftsCount)
		require.NoError(err, "count FTS")
		assert.Equal(0, ftsCount, "FTS count")
	}
}

func TestStore_RemoveSource_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	err := st.RemoveSource(99999)
	requirepkg.Error(t, err, "RemoveSource should error for nonexistent ID")
}

func TestStore_RemoveSource_CascadesConversations(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// Create message with body, raw, and recipients
	msgID := f.CreateMessage("msg-cascade-1")

	err := f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "body text", Valid: true},
		sql.NullString{},
	)
	require.NoError(err, "UpsertMessageBody")

	err = f.Store.UpsertMessageRaw(msgID, []byte("raw MIME data"))
	require.NoError(err, "UpsertMessageRaw")

	pid := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	err = f.Store.ReplaceMessageRecipients(
		msgID, "from", []int64{pid}, []string{"Sender"},
	)
	require.NoError(err, "ReplaceMessageRecipients")

	// Remove source
	err = f.Store.RemoveSource(f.Source.ID)
	require.NoError(err, "RemoveSource")

	// Verify conversations are gone
	var convCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM conversations WHERE source_id = ?`),
		f.Source.ID,
	).Scan(&convCount)
	require.NoError(err, "count conversations")
	assert.Equal(0, convCount, "conversation count")

	// Verify message_bodies are gone (cascaded via messages)
	var bodyCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM message_bodies WHERE message_id = ?`), msgID,
	).Scan(&bodyCount)
	require.NoError(err, "count message_bodies")
	assert.Equal(0, bodyCount, "message_bodies count")

	// Verify message_raw is gone (cascaded via messages)
	var rawCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM message_raw WHERE message_id = ?`), msgID,
	).Scan(&rawCount)
	require.NoError(err, "count message_raw")
	assert.Equal(0, rawCount, "message_raw count")

	// Verify message_recipients are gone (cascaded via messages)
	var recipCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), msgID,
	).Scan(&recipCount)
	require.NoError(err, "count message_recipients")
	assert.Equal(0, recipCount, "message_recipients count")
}

func TestStore_RemoveSourceSerialized_NoActiveSync(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	f.CreateMessage("msg-1")

	had, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.NoError(err, "RemoveSourceSerialized")
	assert.False(had, "hadActiveSync")

	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "source should be removed")
}

func TestStore_RemoveSourceSerialized_ActiveSyncSameSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	f.CreateMessage("msg-1")
	// Active sync on the source being removed — this row would be cascaded
	// by the DELETE. The serialized check must still observe it.
	f.StartSync()

	had, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.NoError(err, "RemoveSourceSerialized")
	assert.True(had, "hadActiveSync should be true for sync on removed source")

	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "source should still be removed even when sync was active")
}

func TestStore_RemoveSourceSerialized_ActiveSyncOtherSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// Create a second source with its own running sync.
	otherSrc, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "create other source")
	_, err = f.Store.StartSync(otherSrc.ID, "full")
	require.NoError(err, "start other sync")

	had, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.NoError(err, "RemoveSourceSerialized")
	assert.True(had, "hadActiveSync should be true for sync on another source")

	// Original source is gone.
	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "test source should be removed")

	// Other source (with the active sync) is untouched.
	other, err := f.Store.GetSourceByIdentifier("other@example.com")
	require.NoError(err, "GetSourceByIdentifier other")
	assert.NotNil(other, "other source should remain")
}

func TestStore_RemoveSourceSerialized_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, err := st.RemoveSourceSerialized(context.Background(), 99999)
	requirepkg.Error(t, err, "RemoveSourceSerialized should error for nonexistent ID")
}

func TestStore_AttachmentPathsUniqueToSource(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)

	// Create a second source with its own conversation.
	otherSrc, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "create other source")
	otherConv, err := f.Store.EnsureConversation(otherSrc.ID, "other-thread", "Other")
	require.NoError(err, "ensure other conv")
	otherMsgID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  otherConv,
		SourceID:        otherSrc.ID,
		SourceMessageID: "other-msg-1",
		MessageType:     "email",
	})
	require.NoError(err, "create other message")

	// Attachment unique to the default source.
	uniqueMsg := f.CreateMessage("msg-unique")
	err = f.Store.UpsertAttachment(uniqueMsg, "u.pdf", "application/pdf",
		"aa/uniquehash", "uniquehash", 10)
	require.NoError(err, "upsert unique attachment")

	// Attachment shared with another source (same content_hash).
	sharedMsg := f.CreateMessage("msg-shared")
	err = f.Store.UpsertAttachment(sharedMsg, "s.pdf", "application/pdf",
		"bb/sharedhash", "sharedhash", 20)
	require.NoError(err, "upsert shared attachment in default source")
	err = f.Store.UpsertAttachment(otherMsgID, "s.pdf", "application/pdf",
		"bb/sharedhash", "sharedhash", 20)
	require.NoError(err, "upsert shared attachment in other source")

	// Attachment with NULL content_hash (must be excluded).
	nullHashMsg := f.CreateMessage("msg-null-hash")
	_, err = f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		 VALUES (?, 'n.pdf', 'application/pdf', 'cc/x', NULL, 30, CURRENT_TIMESTAMP)`),
		nullHashMsg,
	)
	require.NoError(err, "insert null-hash attachment")

	// Attachment with empty storage_path (must be excluded).
	emptyPathMsg := f.CreateMessage("msg-empty-path")
	err = f.Store.UpsertAttachment(emptyPathMsg, "e.pdf", "application/pdf",
		"", "emptypathhash", 40)
	require.NoError(err, "upsert empty-path attachment")

	// Two messages in the default source referencing the same unique hash
	// should collapse to a single storage_path in the result.
	dupMsg := f.CreateMessage("msg-dup-hash")
	err = f.Store.UpsertAttachment(dupMsg, "u.pdf", "application/pdf",
		"aa/uniquehash", "uniquehash", 10)
	require.NoError(err, "upsert duplicate-of-unique attachment")

	paths, err := f.Store.AttachmentPathsUniqueToSource(f.Source.ID)
	require.NoError(err, "AttachmentPathsUniqueToSource")

	require.Len(paths, 1, "paths: %v", paths)
	assertpkg.Equal(t, "aa/uniquehash", paths[0], "path[0]")
}

func TestStore_GetSourceByID(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	got, err := f.Store.GetSourceByID(f.Source.ID)
	require.NoError(err, "GetSourceByID")
	require.NotNil(got, "expected non-nil source")
	assert.Equal(f.Source.ID, got.ID, "ID")
	assert.Equal(f.Source.Identifier, got.Identifier, "Identifier")
}

func TestStore_GetSourceByID_NotFound(t *testing.T) {
	f := storetest.New(t)

	_, err := f.Store.GetSourceByID(99999)
	requirepkg.Error(t, err, "expected error for non-existent ID")
}

func TestStore_IsAttachmentPathReferenced(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-ref-1")
	err := f.Store.UpsertAttachment(msgID, "a.pdf", "application/pdf",
		"aa/hash1", "hash1", 10)
	require.NoError(err, "UpsertAttachment")

	referenced, err := f.Store.IsAttachmentPathReferenced("aa/hash1")
	require.NoError(err, "IsAttachmentPathReferenced (hit)")
	assert.True(referenced, "expected true for referenced path")

	referenced, err = f.Store.IsAttachmentPathReferenced("zz/nothere")
	require.NoError(err, "IsAttachmentPathReferenced (miss)")
	assert.False(referenced, "expected false for unreferenced path")
}

func TestInitSchema_MigratesOAuthAppColumn(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Simulate a pre-migration database that lacks the oauth_app column.
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	// Create the sources table WITHOUT the oauth_app column,
	// matching the schema as it existed before this feature.
	_, err = st.DB().Exec(`
		CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY,
			source_type TEXT NOT NULL,
			identifier TEXT NOT NULL,
			display_name TEXT,
			google_user_id TEXT UNIQUE,
			last_sync_at DATETIME,
			sync_cursor TEXT,
			sync_config JSON,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(source_type, identifier)
		)
	`)
	require.NoError(err, "create legacy sources table")

	// Insert a row into the legacy table.
	_, err = st.DB().Exec(`
		INSERT INTO sources (source_type, identifier, display_name)
		VALUES ('gmail', 'legacy@example.com', 'Legacy User')
	`)
	require.NoError(err, "insert legacy source")

	// Run InitSchema — this should migrate the table by adding oauth_app.
	require.NoError(st.InitSchema(), "InitSchema on legacy DB")

	// Verify GetSourcesByIdentifier works (reads oauth_app column).
	sources, err := st.GetSourcesByIdentifier("legacy@example.com")
	require.NoError(err, "GetSourcesByIdentifier after migration")
	require.Len(sources, 1)
	assert.False(sources[0].OAuthApp.Valid,
		"OAuthApp should be NULL for legacy row, got %q", sources[0].OAuthApp.String)

	// Verify GetSourcesByDisplayName works (also reads oauth_app column).
	sources, err = st.GetSourcesByDisplayName("Legacy User")
	require.NoError(err, "GetSourcesByDisplayName after migration")
	require.Len(sources, 1)

	// Verify oauth_app can be written and read back.
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE sources SET oauth_app = ? WHERE identifier = ?`),
		"acme", "legacy@example.com",
	)
	require.NoError(err, "update oauth_app")

	sources, err = st.GetSourcesByIdentifier("legacy@example.com")
	require.NoError(err, "GetSourcesByIdentifier after update")
	assert.True(sources[0].OAuthApp.Valid, "OAuthApp should be valid after update")
	assert.Equal("acme", sources[0].OAuthApp.String, "OAuthApp value")
}

// TestInitSchema_AddsDeletedAtToLegacyMessagesTable verifies the
// upgrade-path migration: a database whose `messages` table already has
// every other column the embedded schema indexes reference, but is
// missing the dedup-hide column `deleted_at`, gets the column added by
// InitSchema. Without the ALTER, every read path that references
// `deleted_at` (LiveMessagesWhere, the dedup engine, the cache
// staleness check) fails on upgraded databases with "no such column".
func TestInitSchema_AddsDeletedAtToLegacyMessagesTable(t *testing.T) {
	require := requirepkg.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	// Build a messages table that has every column the embedded
	// schema's CREATE INDEX statements reference (sender_id,
	// deleted_from_source_at, message_type, …) but DOES NOT have the
	// new dedup-hide columns (`deleted_at`, `delete_batch_id`).
	// Approximates a legacy DB just before this branch landed.
	_, err = st.DB().Exec(`
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL,
			source_message_id TEXT,
			conversation_id INTEGER,
			subject TEXT,
			snippet TEXT,
			sent_at DATETIME,
			received_at DATETIME,
			internal_date DATETIME,
			size_estimate INTEGER,
			has_attachments BOOLEAN,
			is_from_me BOOLEAN,
			archived_at DATETIME,
			rfc822_message_id TEXT,
			sender_id INTEGER,
			message_type TEXT NOT NULL DEFAULT 'email',
			attachment_count INTEGER DEFAULT 0,
			deleted_from_source_at DATETIME
		)
	`)
	require.NoError(err, "create legacy messages table")

	_, err = st.DB().Exec(`
		INSERT INTO messages (id, source_id, source_message_id, sent_at)
		VALUES (1, 1, 'msg1', datetime('now'))
	`)
	require.NoError(err, "insert legacy message")

	// Run InitSchema — should add deleted_at and delete_batch_id via
	// ALTER TABLE migrations (and silently no-op the columns that
	// already exist, like deleted_from_source_at).
	require.NoError(st.InitSchema(), "InitSchema on legacy DB")

	// Confirm the canonical live-messages predicate runs without
	// "no such column": this is the failure mode codex flagged. The
	// query uses both deleted_at and deleted_from_source_at.
	var n int
	require.NoError(st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE "+store.LiveMessagesWhere("", true),
	).Scan(&n), "post-migration live count")
	assertpkg.Equal(t, 1, n, "post-migration live count")

	// Confirm delete_batch_id is also queryable post-migration so
	// DeleteAllDeduped's distinct-batch count works on upgraded DBs.
	_, err = st.DB().Exec(
		"SELECT COUNT(DISTINCT delete_batch_id) FROM messages",
	)
	require.NoError(err, "post-migration delete_batch_id query")
}
