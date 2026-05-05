package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

func TestStore_GetSourcesByIdentifier(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Create two sources with same identifier, different types
	_, err := st.GetOrCreateSource("gmail", "user@example.com")
	testutil.MustNoErr(t, err, "create gmail source")
	_, err = st.GetOrCreateSource("mbox", "user@example.com")
	testutil.MustNoErr(t, err, "create mbox source")

	sources, err := st.GetSourcesByIdentifier("user@example.com")
	testutil.MustNoErr(t, err, "GetSourcesByIdentifier")
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(sources))
	}

	// Verify ordering by source_type
	if sources[0].SourceType != "gmail" {
		t.Errorf("sources[0].SourceType = %q, want gmail", sources[0].SourceType)
	}
	if sources[1].SourceType != "mbox" {
		t.Errorf("sources[1].SourceType = %q, want mbox", sources[1].SourceType)
	}
}

func TestStore_GetSourcesByIdentifier_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	sources, err := st.GetSourcesByIdentifier("nobody@example.com")
	testutil.MustNoErr(t, err, "GetSourcesByIdentifier")
	if len(sources) != 0 {
		t.Errorf("got %d sources, want 0", len(sources))
	}
}

func TestStore_RemoveSource(t *testing.T) {
	f := storetest.New(t)

	// Create messages, labels, and FTS data
	msgID := f.CreateMessage("msg-remove-1")
	f.CreateMessage("msg-remove-2")

	labels := f.EnsureLabels(map[string]string{
		"INBOX": "Inbox",
	}, "system")
	err := f.Store.ReplaceMessageLabels(msgID, []int64{labels["INBOX"]})
	testutil.MustNoErr(t, err, "ReplaceMessageLabels")

	if f.Store.FTS5Available() {
		err = f.Store.UpsertFTS(msgID, "Test", "body", "a@b.com", "", "")
		testutil.MustNoErr(t, err, "UpsertFTS")
	}

	// Remove source
	err = f.Store.RemoveSource(f.Source.ID)
	testutil.MustNoErr(t, err, "RemoveSource")

	// Verify source is gone
	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier")
	if src != nil {
		t.Error("source should be nil after removal")
	}

	// Verify messages are gone
	count, err := f.Store.CountMessagesForSource(f.Source.ID)
	testutil.MustNoErr(t, err, "CountMessagesForSource")
	if count != 0 {
		t.Errorf("message count = %d, want 0", count)
	}

	// Verify labels are gone
	var labelCount int
	err = f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM labels WHERE source_id = ?`, f.Source.ID,
	).Scan(&labelCount)
	testutil.MustNoErr(t, err, "count labels")
	if labelCount != 0 {
		t.Errorf("label count = %d, want 0", labelCount)
	}

	// Verify FTS rows are gone
	if f.Store.FTS5Available() {
		var ftsCount int
		err = f.Store.DB().QueryRow(
			`SELECT COUNT(*) FROM messages_fts`,
		).Scan(&ftsCount)
		testutil.MustNoErr(t, err, "count FTS")
		if ftsCount != 0 {
			t.Errorf("FTS count = %d, want 0", ftsCount)
		}
	}
}

func TestStore_RemoveSource_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	err := st.RemoveSource(99999)
	if err == nil {
		t.Fatal("RemoveSource should error for nonexistent ID")
	}
}

func TestStore_RemoveSource_CascadesConversations(t *testing.T) {
	f := storetest.New(t)

	// Create message with body, raw, and recipients
	msgID := f.CreateMessage("msg-cascade-1")

	err := f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "body text", Valid: true},
		sql.NullString{},
	)
	testutil.MustNoErr(t, err, "UpsertMessageBody")

	err = f.Store.UpsertMessageRaw(msgID, []byte("raw MIME data"))
	testutil.MustNoErr(t, err, "UpsertMessageRaw")

	pid := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	err = f.Store.ReplaceMessageRecipients(
		msgID, "from", []int64{pid}, []string{"Sender"},
	)
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients")

	// Remove source
	err = f.Store.RemoveSource(f.Source.ID)
	testutil.MustNoErr(t, err, "RemoveSource")

	// Verify conversations are gone
	var convCount int
	err = f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE source_id = ?`,
		f.Source.ID,
	).Scan(&convCount)
	testutil.MustNoErr(t, err, "count conversations")
	if convCount != 0 {
		t.Errorf("conversation count = %d, want 0", convCount)
	}

	// Verify message_bodies are gone (cascaded via messages)
	var bodyCount int
	err = f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM message_bodies WHERE message_id = ?`, msgID,
	).Scan(&bodyCount)
	testutil.MustNoErr(t, err, "count message_bodies")
	if bodyCount != 0 {
		t.Errorf("message_bodies count = %d, want 0", bodyCount)
	}

	// Verify message_raw is gone (cascaded via messages)
	var rawCount int
	err = f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM message_raw WHERE message_id = ?`, msgID,
	).Scan(&rawCount)
	testutil.MustNoErr(t, err, "count message_raw")
	if rawCount != 0 {
		t.Errorf("message_raw count = %d, want 0", rawCount)
	}

	// Verify message_recipients are gone (cascaded via messages)
	var recipCount int
	err = f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`, msgID,
	).Scan(&recipCount)
	testutil.MustNoErr(t, err, "count message_recipients")
	if recipCount != 0 {
		t.Errorf("message_recipients count = %d, want 0", recipCount)
	}
}

func TestStore_RemoveSourceSerialized_NoActiveSync(t *testing.T) {
	f := storetest.New(t)
	f.CreateMessage("msg-1")

	had, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	testutil.MustNoErr(t, err, "RemoveSourceSerialized")
	if had {
		t.Error("hadActiveSync = true, want false")
	}

	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier")
	if src != nil {
		t.Error("source should be removed")
	}
}

func TestStore_RemoveSourceSerialized_ActiveSyncSameSource(t *testing.T) {
	f := storetest.New(t)
	f.CreateMessage("msg-1")
	// Active sync on the source being removed — this row would be cascaded
	// by the DELETE. The serialized check must still observe it.
	f.StartSync()

	had, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	testutil.MustNoErr(t, err, "RemoveSourceSerialized")
	if !had {
		t.Error("hadActiveSync = false, want true for sync on removed source")
	}

	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier")
	if src != nil {
		t.Error("source should still be removed even when sync was active")
	}
}

func TestStore_RemoveSourceSerialized_ActiveSyncOtherSource(t *testing.T) {
	f := storetest.New(t)

	// Create a second source with its own running sync.
	otherSrc, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	testutil.MustNoErr(t, err, "create other source")
	_, err = f.Store.StartSync(otherSrc.ID, "full")
	testutil.MustNoErr(t, err, "start other sync")

	had, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	testutil.MustNoErr(t, err, "RemoveSourceSerialized")
	if !had {
		t.Error("hadActiveSync = false, want true for sync on another source")
	}

	// Original source is gone.
	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier")
	if src != nil {
		t.Error("test source should be removed")
	}

	// Other source (with the active sync) is untouched.
	other, err := f.Store.GetSourceByIdentifier("other@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier other")
	if other == nil {
		t.Error("other source should remain")
	}
}

func TestStore_RemoveSourceSerialized_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, err := st.RemoveSourceSerialized(context.Background(), 99999)
	if err == nil {
		t.Fatal("RemoveSourceSerialized should error for nonexistent ID")
	}
}

func TestStore_AttachmentPathsUniqueToSource(t *testing.T) {
	f := storetest.New(t)

	// Create a second source with its own conversation.
	otherSrc, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	testutil.MustNoErr(t, err, "create other source")
	otherConv, err := f.Store.EnsureConversation(otherSrc.ID, "other-thread", "Other")
	testutil.MustNoErr(t, err, "ensure other conv")
	otherMsgID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  otherConv,
		SourceID:        otherSrc.ID,
		SourceMessageID: "other-msg-1",
		MessageType:     "email",
	})
	testutil.MustNoErr(t, err, "create other message")

	// Attachment unique to the default source.
	uniqueMsg := f.CreateMessage("msg-unique")
	err = f.Store.UpsertAttachment(uniqueMsg, "u.pdf", "application/pdf",
		"aa/uniquehash", "uniquehash", 10)
	testutil.MustNoErr(t, err, "upsert unique attachment")

	// Attachment shared with another source (same content_hash).
	sharedMsg := f.CreateMessage("msg-shared")
	err = f.Store.UpsertAttachment(sharedMsg, "s.pdf", "application/pdf",
		"bb/sharedhash", "sharedhash", 20)
	testutil.MustNoErr(t, err, "upsert shared attachment in default source")
	err = f.Store.UpsertAttachment(otherMsgID, "s.pdf", "application/pdf",
		"bb/sharedhash", "sharedhash", 20)
	testutil.MustNoErr(t, err, "upsert shared attachment in other source")

	// Attachment with NULL content_hash (must be excluded).
	nullHashMsg := f.CreateMessage("msg-null-hash")
	_, err = f.Store.DB().Exec(
		`INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		 VALUES (?, 'n.pdf', 'application/pdf', 'cc/x', NULL, 30, CURRENT_TIMESTAMP)`,
		nullHashMsg,
	)
	testutil.MustNoErr(t, err, "insert null-hash attachment")

	// Attachment with empty storage_path (must be excluded).
	emptyPathMsg := f.CreateMessage("msg-empty-path")
	err = f.Store.UpsertAttachment(emptyPathMsg, "e.pdf", "application/pdf",
		"", "emptypathhash", 40)
	testutil.MustNoErr(t, err, "upsert empty-path attachment")

	// Two messages in the default source referencing the same unique hash
	// should collapse to a single storage_path in the result.
	dupMsg := f.CreateMessage("msg-dup-hash")
	err = f.Store.UpsertAttachment(dupMsg, "u.pdf", "application/pdf",
		"aa/uniquehash", "uniquehash", 10)
	testutil.MustNoErr(t, err, "upsert duplicate-of-unique attachment")

	paths, err := f.Store.AttachmentPathsUniqueToSource(f.Source.ID)
	testutil.MustNoErr(t, err, "AttachmentPathsUniqueToSource")

	if len(paths) != 1 {
		t.Fatalf("got %d paths, want 1: %v", len(paths), paths)
	}
	if paths[0] != "aa/uniquehash" {
		t.Errorf("path[0] = %q, want aa/uniquehash", paths[0])
	}
}

func TestStore_GetSourceByID(t *testing.T) {
	f := storetest.New(t)

	got, err := f.Store.GetSourceByID(f.Source.ID)
	testutil.MustNoErr(t, err, "GetSourceByID")
	if got == nil {
		t.Fatal("expected non-nil source")
	}
	if got.ID != f.Source.ID {
		t.Errorf("ID = %d, want %d", got.ID, f.Source.ID)
	}
	if got.Identifier != f.Source.Identifier {
		t.Errorf("Identifier = %q, want %q", got.Identifier, f.Source.Identifier)
	}
}

func TestStore_GetSourceByID_NotFound(t *testing.T) {
	f := storetest.New(t)

	_, err := f.Store.GetSourceByID(99999)
	if err == nil {
		t.Fatal("expected error for non-existent ID, got nil")
	}
}

func TestStore_IsAttachmentPathReferenced(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-ref-1")
	err := f.Store.UpsertAttachment(msgID, "a.pdf", "application/pdf",
		"aa/hash1", "hash1", 10)
	testutil.MustNoErr(t, err, "UpsertAttachment")

	referenced, err := f.Store.IsAttachmentPathReferenced("aa/hash1")
	testutil.MustNoErr(t, err, "IsAttachmentPathReferenced (hit)")
	if !referenced {
		t.Error("expected true for referenced path")
	}

	referenced, err = f.Store.IsAttachmentPathReferenced("zz/nothere")
	testutil.MustNoErr(t, err, "IsAttachmentPathReferenced (miss)")
	if referenced {
		t.Error("expected false for unreferenced path")
	}
}

func TestInitSchema_MigratesOAuthAppColumn(t *testing.T) {
	// Simulate a pre-migration database that lacks the oauth_app column.
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
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
	if err != nil {
		t.Fatalf("create legacy sources table: %v", err)
	}

	// Insert a row into the legacy table.
	_, err = st.DB().Exec(`
		INSERT INTO sources (source_type, identifier, display_name)
		VALUES ('gmail', 'legacy@example.com', 'Legacy User')
	`)
	if err != nil {
		t.Fatalf("insert legacy source: %v", err)
	}

	// Run InitSchema — this should migrate the table by adding oauth_app.
	if err := st.InitSchema(); err != nil {
		t.Fatalf("InitSchema on legacy DB: %v", err)
	}

	// Verify GetSourcesByIdentifier works (reads oauth_app column).
	sources, err := st.GetSourcesByIdentifier("legacy@example.com")
	if err != nil {
		t.Fatalf("GetSourcesByIdentifier after migration: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	if sources[0].OAuthApp.Valid {
		t.Errorf("OAuthApp should be NULL for legacy row, got %q", sources[0].OAuthApp.String)
	}

	// Verify GetSourcesByDisplayName works (also reads oauth_app column).
	sources, err = st.GetSourcesByDisplayName("Legacy User")
	if err != nil {
		t.Fatalf("GetSourcesByDisplayName after migration: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}

	// Verify oauth_app can be written and read back.
	_, err = st.DB().Exec(
		`UPDATE sources SET oauth_app = ? WHERE identifier = ?`,
		"acme", "legacy@example.com",
	)
	if err != nil {
		t.Fatalf("update oauth_app: %v", err)
	}

	sources, err = st.GetSourcesByIdentifier("legacy@example.com")
	if err != nil {
		t.Fatalf("GetSourcesByIdentifier after update: %v", err)
	}
	if !sources[0].OAuthApp.Valid || sources[0].OAuthApp.String != "acme" {
		t.Errorf("OAuthApp = %v, want {acme true}", sources[0].OAuthApp)
	}
}

// TestInitSchema_AddsDeletedAtToLegacyMessagesTable verifies the
// upgrade-path migration: a database whose `messages` table already has
// every other column the embedded schema indexes reference, but is
// missing the dedup-hide column `deleted_at`, gets the column added by
// InitSchema. Without the ALTER, every read path that references
// `deleted_at` (LiveMessagesWhere, the dedup engine, the cache
// staleness check) fails on upgraded databases with "no such column".
func TestInitSchema_AddsDeletedAtToLegacyMessagesTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Build a messages table that has every column the embedded
	// schema's CREATE INDEX statements reference (sender_id,
	// deleted_from_source_at, message_type, …) but DOES NOT have the
	// new dedup-hide columns (`deleted_at`, `delete_batch_id`).
	// Approximates a legacy DB just before this branch landed.
	if _, err := st.DB().Exec(`
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
	`); err != nil {
		t.Fatalf("create legacy messages table: %v", err)
	}

	if _, err := st.DB().Exec(`
		INSERT INTO messages (id, source_id, source_message_id, sent_at)
		VALUES (1, 1, 'msg1', datetime('now'))
	`); err != nil {
		t.Fatalf("insert legacy message: %v", err)
	}

	// Run InitSchema — should add deleted_at and delete_batch_id via
	// ALTER TABLE migrations (and silently no-op the columns that
	// already exist, like deleted_from_source_at).
	if err := st.InitSchema(); err != nil {
		t.Fatalf("InitSchema on legacy DB: %v", err)
	}

	// Confirm the canonical live-messages predicate runs without
	// "no such column": this is the failure mode codex flagged. The
	// query uses both deleted_at and deleted_from_source_at.
	var n int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE " + store.LiveMessagesWhere("", true),
	).Scan(&n); err != nil {
		t.Fatalf("post-migration live count: %v", err)
	}
	if n != 1 {
		t.Errorf("post-migration live count = %d, want 1", n)
	}

	// Confirm delete_batch_id is also queryable post-migration so
	// DeleteAllDeduped's distinct-batch count works on upgraded DBs.
	if _, err := st.DB().Exec(
		"SELECT COUNT(DISTINCT delete_batch_id) FROM messages",
	); err != nil {
		t.Fatalf("post-migration delete_batch_id query: %v", err)
	}
}
