package imessage

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func attachmentFixture(t *testing.T, policy attachmentpolicy.Policy) (*Client, *sql.DB, *store.Store, int64, string, string) {
	t.Helper()
	path, db := newChatDB(t)
	_, err := db.Exec(`
 ALTER TABLE message ADD COLUMN is_audio_message INTEGER;
 ALTER TABLE message ADD COLUMN audio_message_duration REAL;
 CREATE TABLE attachment (guid TEXT, filename TEXT, transfer_name TEXT, mime_type TEXT, uti TEXT, total_bytes INTEGER);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
 INSERT INTO message (date,text,is_audio_message,audio_message_duration) VALUES (700000000,'Voice',1,1.25),(700000001,'Shared',0,NULL);
 INSERT INTO chat VALUES ('any;-;synthetic','', 'synthetic');
 INSERT INTO chat_message_join VALUES (1,1),(1,2);
 INSERT INTO chat_handle_join VALUES (1,1);
 INSERT INTO attachment VALUES ('synthetic-a','~/Library/Messages/Attachments/voice.caf','../../voice.caf','audio/x-caf','com.apple.coreaudio-format',4);
 INSERT INTO message_attachment_join VALUES (1,1),(1,1),(2,1);
 `)
	require.NoError(t, err)
	root, dest := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "voice.caf"), []byte("caffSYNTHETIC"), 0600))
	c, err := NewClient(path, WithAttachments(dest, root, policy))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(t, err)
	return c, db, st, src.ID, root, dest
}

func importedAttachment(t *testing.T, st *store.Store, source int64, message string) (int64, store.AttachmentRef) {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), source, message).Scan(&id))
	refs, err := st.MessageIMessageAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	return id, refs["imessage:1"]
}

// Dropping joined attachments, trusting cache flags, or hashing filenames breaks this.
func TestImportAttachmentBytesAndIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, root, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	var original int64
	for run := range 3 {
		if run == 1 {
			require.NoError(os.Remove(filepath.Join(root, "voice.caf")))
		}
		if run == 2 {
			_, err := db.Exec(`DELETE FROM message_attachment_join`)
			require.NoError(err)
		}
		summary, err := c.Import(t.Context(), st, source)
		require.NoError(err)
		assert.Equal(2, summary.MessagesImported)
		id, first := importedAttachment(t, st, source, "1")
		_, second := importedAttachment(t, st, source, "2")
		assert.Equal(first.ContentHash, second.ContentHash)
		assert.Equal(attachmentpolicy.StateStored, first.State)
		assert.Equal("voice.caf", first.Filename)
		assert.Equal("voice_note", first.MediaType)
		assert.Equal("audio", second.MediaType)
		assert.Equal(13, first.Size)
		b, err := os.ReadFile(filepath.Join(dest, first.StoragePath))
		require.NoError(err)
		assert.Equal("caffSYNTHETIC", string(b))
		var metadata map[string]any
		require.NoError(json.Unmarshal([]byte(first.Metadata), &metadata))
		assert.Equal("com.apple.coreaudio-format", metadata["uti"])
		assert.InDelta(1.25, metadata["audio_message_duration"], 0.001)
		var row int64
		var has bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM attachments WHERE message_id=?`), id).Scan(&row))
		if run == 0 {
			original = row
		}
		assert.Equal(original, row)
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT has_attachments FROM messages WHERE id=?`), id).Scan(&has))
		assert.True(has)
		candidates, err := st.ListBeeperMediaCandidates(t.Context(), 0, 100)
		require.NoError(err)
		assert.Len(candidates, 2)
	}
}

// Unavailable markers must remain visible and recover on an explicit rerun.
func TestImportUnavailableAttachmentRecovers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, _, st, source, root, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	require.NoError(os.Remove(filepath.Join(root, "voice.caf")))
	_, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	_, ref := importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateUnavailable, ref.State)
	assert.Equal(attachmentpolicy.SkipSourceUnavailable, ref.SkipReason)
	assert.Empty(ref.ContentHash)
	candidates, err := st.ListBeeperMediaCandidates(t.Context(), 0, 100)
	require.NoError(err)
	assert.Empty(candidates)
	require.NoError(os.WriteFile(filepath.Join(root, "voice.caf"), []byte("restored"), 0600))
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, ref = importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateStored, ref.State)
}

func TestImportAttachmentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   attachmentpolicy.Policy
		declared int
		reason   attachmentpolicy.SkipReason
	}{
		{"scope", attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}, 4, attachmentpolicy.SkipPolicyScope},
		{"roster", attachmentpolicy.Policy{MaxParticipants: 1}, 4, attachmentpolicy.SkipParticipantThreshold},
		{"declared size", attachmentpolicy.Policy{MaxBytes: 5}, 20, attachmentpolicy.SkipSizeCap},
		{"actual size", attachmentpolicy.Policy{MaxBytes: 5}, 4, attachmentpolicy.SkipSizeCap},
		{"default size", attachmentpolicy.Policy{}, 250<<20 + 1, attachmentpolicy.SkipSizeCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, db, st, source, _, dest := attachmentFixture(t, tc.policy)
			_, err := db.Exec(`UPDATE attachment SET total_bytes=?`, tc.declared)
			require.NoError(err)
			_, err = c.Import(t.Context(), st, source)
			require.NoError(err)
			_, ref := importedAttachment(t, st, source, "1")
			assert.Equal(attachmentpolicy.StateSkipped, ref.State)
			assert.Equal(tc.reason, ref.SkipReason)
			entries, err := os.ReadDir(dest)
			require.NoError(err)
			assert.Empty(entries)
		})
	}
}

func TestImportAttachmentFailures(t *testing.T) {
	t.Run("message write returns error", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		testutil.SkipIfPostgres(t, "SQLite trigger injection; portable attachment identity is covered in store tests")
		c, _, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
		c.pageSize = 1
		c.logger = slog.New(slog.DiscardHandler)
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_message_failure BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic message failure'); END`)
		require.NoError(err)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		_, err = c.Import(ctx, st, source)
		require.ErrorContains(err, "synthetic message failure")
		assert.NoError(ctx.Err(), "the write failure must return without requiring cancellation")
	})
	t.Run("archive write returns error", func(t *testing.T) {
		require := require.New(t)
		testutil.SkipIfPostgres(t, "SQLite trigger injection; portable attachment identity is covered in store tests")
		c, _, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
		c.pageSize = 1
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_attachment_failure BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
		require.NoError(err)
		_, err = c.Import(t.Context(), st, source)
		require.ErrorContains(err, "synthetic failure")
	})
	t.Run("broken schema rejected", func(t *testing.T) {
		require := require.New(t)
		c, db, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
		_, err := db.Exec(`DROP TABLE message_attachment_join`)
		require.NoError(err)
		_, err = c.Import(t.Context(), st, source)
		require.Error(err)
	})
}

func TestImportAttachmentChangedGUIDDoesNotReuseBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, root, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	_, before := importedAttachment(t, st, source, "1")
	_, err = db.Exec(`UPDATE attachment SET guid = 'synthetic-replacement'`)
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(root, "voice.caf"), []byte("replacement"), 0600))
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, after := importedAttachment(t, st, source, "1")
	assert.NotEqual(before.ContentHash, after.ContentHash)
	b, err := os.ReadFile(filepath.Join(dest, after.StoragePath))
	require.NoError(err)
	assert.Equal("replacement", string(b))
}

func TestImportCapturedAttachmentsWithoutSourceTables(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	_, err = db.Exec(`DROP TABLE attachment; DROP TABLE message_attachment_join`)
	require.NoError(err)
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	id, ref := importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateStored, ref.State)
	var has bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT has_attachments FROM messages WHERE id=?`), id).Scan(&has))
	assert.True(has)
}

func TestImportCapturedMetadataRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, root, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(root, "voice.caf")))
	_, err = db.Exec(`UPDATE attachment SET transfer_name = 'renamed.caf'`)
	require.NoError(err)
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, ref := importedAttachment(t, st, source, "1")
	assert.Equal("renamed.caf", ref.Filename)
	assert.Equal(attachmentpolicy.StateStored, ref.State)
}

func TestClientReadsNonWALCopy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, db := newChatDB(t)
	_, err := db.Exec(`PRAGMA journal_mode=DELETE`)
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	var mode string
	require.NoError(db.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	assert.Equal("delete", mode)
}

func TestImportReusedRowWithoutGUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := db.Exec(`UPDATE attachment SET guid = NULL`)
	require.NoError(err)
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, err = db.Exec(`UPDATE attachment SET filename = 'missing-replacement.caf'`)
	require.NoError(err)
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, ref := importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateUnavailable, ref.State)
	assert.Empty(ref.ContentHash)
}

func TestImportGUIDlessStoredAttachmentSurvivesSourceDisappearance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, root, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := db.Exec(`UPDATE attachment SET guid = NULL`)
	require.NoError(err)
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	messageID, before := importedAttachment(t, st, source, "1")

	// Formatting changes as JSONB normalizes the metadata, without changing its value.
	updateMetadata := `UPDATE attachments SET attachment_metadata=?`
	if st.IsPostgreSQL() {
		updateMetadata = `UPDATE attachments SET attachment_metadata=?::jsonb`
	}
	_, err = st.DB().Exec(
		st.Rebind(updateMetadata+` WHERE message_id=? AND source_attachment_id=?`),
		" "+before.Metadata+" ", messageID, before.SourceAttachmentID,
	)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(root, "voice.caf")))
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)

	_, after := importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateStored, after.State)
	assert.Equal(before.ContentHash, after.ContentHash)
	contents, err := os.ReadFile(filepath.Join(dest, after.StoragePath))
	require.NoError(err)
	assert.Equal("caffSYNTHETIC", string(contents))
}

func TestImportDegenerateTransferName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	for _, name := range []string{"..", "sub/.."} {
		_, err := db.Exec(`UPDATE attachment SET transfer_name = ?`, name)
		require.NoError(err)
		_, err = c.Import(t.Context(), st, source)
		require.NoError(err)
		_, ref := importedAttachment(t, st, source, "1")
		assert.Equal("attachment", ref.Filename)
	}
}
