package imessage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type importFixtureParts struct {
	path       string
	c          *Client
	db         *sql.DB
	st         *store.Store
	source     int64
	root, dest string
}

func newImportFixture(t *testing.T, policy attachmentpolicy.Policy) importFixtureParts {
	t.Helper()
	path, db := testutil.NewIMessageAttachmentChatDB(t)
	root, dest := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Attachments"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Attachments", "voice.caf"), []byte("caffSYNTHETIC"), 0600))
	c, err := NewClient(path, WithAttachments(dest, root, policy))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(t, err)
	return importFixtureParts{path, c, db, st, src.ID, root, dest}
}

func attachmentFixture(t *testing.T, policy attachmentpolicy.Policy) (*Client, *sql.DB, *store.Store, int64, string, string) {
	t.Helper()
	f := newImportFixture(t, policy)
	return f.c, f.db, f.st, f.source, f.root, f.dest
}

func importOnlyFixture(t *testing.T) (*Client, *store.Store, int64) {
	t.Helper()
	f := newImportFixture(t, attachmentpolicy.Policy{})
	return f.c, f.st, f.source
}

func importedAttachment(t *testing.T, st *store.Store, source int64, message string) (int64, store.AttachmentRef) {
	t.Helper()
	return importedAttachmentByKey(t, st, source, message, "imessage:1")
}

func importedAttachmentByKey(t *testing.T, st *store.Store, source int64, message, key string) (int64, store.AttachmentRef) {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), source, message).Scan(&id))
	refs, err := st.MessageIMessageAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Contains(t, refs, key)
	return id, refs[key]
}

// Dropping joined attachments, trusting the cache flag, or hashing filenames breaks this.
func TestImportAttachmentBytesAndIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := db.Exec(`INSERT INTO attachment VALUES ('synthetic-sticker','~/Library/Messages/Attachments/voice.caf','sticker.caf','audio/x-caf','com.apple.coreaudio-format',13,1);
		INSERT INTO message (date,text) VALUES (700000002,'Sticker');
		INSERT INTO chat_message_join VALUES (1,3);
		INSERT INTO message_attachment_join VALUES (3,2);
		UPDATE message SET cache_has_attachments = 0`)
	require.NoError(err)
	summary, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	assert.Equal(3, summary.MessagesImported)
	_, first := importedAttachment(t, st, source, "1")
	_, second := importedAttachment(t, st, source, "2")
	assert.Equal(first.ContentHash, second.ContentHash)
	assert.Equal(attachmentpolicy.StateStored, first.State)
	assert.Equal(store.AttachmentRoleStandalone, first.Role)
	assert.Equal("audio", first.MediaType)
	assert.Equal("audio", second.MediaType)
	assert.Equal(13, first.Size)
	b, err := os.ReadFile(filepath.Join(dest, first.StoragePath))
	require.NoError(err)
	assert.Equal("caffSYNTHETIC", string(b))
	var metadata map[string]any
	require.NoError(json.Unmarshal([]byte(first.Metadata), &metadata))
	assert.Equal("com.apple.coreaudio-format", metadata["uti"])

	var stickerMessage int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id='3'`), source).Scan(&stickerMessage))
	refs, err := st.MessageIMessageAttachmentsContext(t.Context(), stickerMessage)
	require.NoError(err)
	require.Contains(refs, "imessage:2")
	assert.Equal(store.AttachmentRoleSticker, refs["imessage:2"].Role)
}

func TestImportUnchangedRerunDoesNotRewriteAttachments(t *testing.T) {
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	require := require.New(t)
	assert := assert.New(t)
	f := newImportFixture(t, attachmentpolicy.Policy{})
	c, st, source := f.c, f.st, f.source
	_, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	messageID, _ := importedAttachment(t, st, source, "1")

	var baseline int64
	require.NoError(st.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal
		WHERE dataset = 'attachments' AND message_id = ?`, messageID).Scan(&baseline))
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)

	var changes int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal
		WHERE seq > ? AND dataset = 'attachments' AND message_id = ?`, baseline, messageID).Scan(&changes))
	assert.Zero(changes, "an unchanged reimport must not schedule attachment cache repairs")

	require.NoError(os.Remove(filepath.Join(f.root, "Attachments", "voice.caf")))
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal
		WHERE seq > ? AND dataset = 'attachments' AND message_id = ?`, baseline, messageID).Scan(&changes))
	assert.Zero(changes, "a rerun after the source file is gone must keep the archived row untouched")
	_, ref := importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateStored, ref.State)
}

func TestImportAttachmentSchema(t *testing.T) {
	const (
		dropJoin = `DROP TABLE message_attachment_join`
		dropBoth = `DROP TABLE attachment; DROP TABLE message_attachment_join`
	)
	for _, tc := range []struct {
		name    string
		drop    string
		capture bool
		wantErr string
	}{
		{"metadata-only ignores an unsupported schema", dropJoin, false, ""},
		{"capture rejects an unsupported schema", dropJoin, true, "unsupported attachment schema"},
		{"capture rejects missing attachment tables", dropBoth, true, "unsupported attachment schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path, db := testutil.NewIMessageAttachmentChatDB(t)
			if tc.drop != "" {
				_, err := db.Exec(tc.drop)
				require.NoError(err)
			}
			directory, root := "", ""
			if tc.capture {
				directory, root = t.TempDir(), t.TempDir()
			}
			c, err := NewClient(path, WithAttachments(directory, root, attachmentpolicy.Policy{}))
			require.NoError(err)
			t.Cleanup(func() { _ = c.Close() })
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("apple_messages", "local")
			require.NoError(err)
			summary, err := c.Import(t.Context(), st, src.ID)
			if tc.wantErr != "" {
				require.ErrorContains(err, tc.wantErr)
				// The error must tell the user how to carry on.
				require.ErrorContains(err, "--no-attachments")
				return
			}
			require.NoError(err)
			assert.Equal(2, summary.MessagesImported)
		})
	}
}

// cancelOnceStored reports cancellation as soon as the first message is archived.
type cancelOnceStored struct {
	context.Context

	db *sql.DB
}

func (c cancelOnceStored) Err() error {
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err == nil && n > 0 {
		return context.Canceled
	}
	return nil
}

func TestImportCancellationIsNotAnAttachmentFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, st, source := importOnlyFixture(t)
	summary, err := c.Import(cancelOnceStored{Context: t.Context(), db: st.DB()}, st, source)
	require.ErrorIs(err, context.Canceled)
	require.NotErrorIs(err, ErrAttachmentImport)
	assert.Zero(summary.AttachmentFailures)
}

// Unavailable markers must remain visible and recover on an explicit rerun.
func TestImportUnavailableAttachmentRecovers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, _, st, source, root, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	require.NoError(os.Remove(filepath.Join(root, "Attachments", "voice.caf")))
	summary, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	assert.Equal(2, summary.AttachmentsUnavailable, "a clean-looking import must still count unreadable files")
	_, ref := importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateUnavailable, ref.State)
	assert.Equal(attachmentpolicy.SkipSourceUnavailable, ref.SkipReason)
	assert.Empty(ref.ContentHash)
	require.NoError(os.WriteFile(filepath.Join(root, "Attachments", "voice.caf"), []byte("restored"), 0600))
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, ref = importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateStored, ref.State)
}

func TestImportAttachmentOccurrencesUseSourceRowIDOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := db.Exec(`DELETE FROM message_attachment_join WHERE message_id = 1`)
	require.NoError(err)
	want := make([]string, 0, 10)
	for id := int64(1); id <= 10; id++ {
		if id > 1 {
			_, err = db.Exec(`INSERT INTO attachment (ROWID, guid, filename, transfer_name, mime_type, uti, total_bytes)
				VALUES (?, ?, '~/Library/Messages/Attachments/voice.caf', 'voice.caf', 'audio/x-caf', 'com.apple.coreaudio-format', 4)`,
				id, "synthetic-"+strconv.FormatInt(id, 10))
			require.NoError(err)
		}
		_, err = db.Exec(`INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (1, ?)`, id)
		require.NoError(err)
		want = append(want, "imessage:"+strconv.FormatInt(id, 10))
	}
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		source, "1").Scan(&messageID))
	rows, err := st.DB().Query(st.Rebind(`SELECT source_attachment_id FROM attachments WHERE message_id = ? ORDER BY id`), messageID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var key string
		require.NoError(rows.Scan(&key))
		got = append(got, key)
	}
	require.NoError(rows.Err())
	assert.Equal(want, got)
}

func TestImportAttachmentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   attachmentpolicy.Policy
		declared int
		filename string
		reason   attachmentpolicy.SkipReason
	}{
		{"roster", attachmentpolicy.Policy{MaxParticipants: 1}, 4, "", attachmentpolicy.SkipParticipantThreshold},
		{"actual size", attachmentpolicy.Policy{MaxBytes: 5}, 4, "", attachmentpolicy.SkipSizeCap},
		{"default size", attachmentpolicy.Policy{}, 250<<20 + 1, "", attachmentpolicy.SkipSizeCap},
		{"path outside the root", attachmentpolicy.Policy{}, 4, "../outside.caf", attachmentpolicy.SkipAccountPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, db, st, source, _, dest := attachmentFixture(t, tc.policy)
			_, err := db.Exec(`UPDATE attachment SET total_bytes=?`, tc.declared)
			require.NoError(err)
			if tc.filename != "" {
				_, err = db.Exec(`UPDATE attachment SET filename=?`, tc.filename)
				require.NoError(err)
			}
			summary, err := c.Import(t.Context(), st, source)
			require.NoError(err)
			assert.Equal(2, summary.AttachmentsSkipped)
			assert.Zero(summary.AttachmentsUnavailable, "a skipped attachment is not a Full Disk Access problem")
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
	t.Run("archive write error stops the run", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		testutil.SkipIfPostgres(t, "SQLite trigger injection; portable attachment identity is covered in store tests")
		c, _, st, source, _, dest := attachmentFixture(t, attachmentpolicy.Policy{})
		c.pageSize = 1
		require.NoError(os.RemoveAll(dest))
		require.NoError(os.WriteFile(dest, []byte("synthetic destination blocker"), 0600))
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_attachment_failure BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
		require.NoError(err)
		_, err = c.Import(t.Context(), st, source)
		require.ErrorContains(err, "synthetic failure")
		require.ErrorIs(err, ErrAttachmentImport)
		require.ErrorContains(err, "store iMessage attachment bytes")
		var messages int
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source).Scan(&messages))
		assert.Equal(1, messages, "a failing archive write must not be retried for every later message")
		var messageCount int
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_count FROM conversations WHERE source_id = ?`), source).Scan(&messageCount))
		assert.Equal(1, messageCount, "the committed message must be counted before the fatal error returns")
	})
}

// A reimport that cannot refresh a stored occurrence keeps its archived bytes when the
// source attachment is the same one, and reports it unavailable when it is not.
func TestImportRetainsArchivedBytesByAttachmentIdentity(t *testing.T) {
	removeSource := func(t *testing.T, f importFixtureParts, _ store.AttachmentRef) *Client {
		t.Helper()
		require.NoError(t, os.Remove(filepath.Join(f.root, "Attachments", "voice.caf")))
		return f.c
	}
	metadataOnly := func(t *testing.T, f importFixtureParts) *Client {
		t.Helper()
		c, err := NewClient(f.path, WithAttachments("", "", attachmentpolicy.Policy{}))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	for _, tc := range []struct {
		name, setup, update string
		policy              attachmentpolicy.Policy
		reimport            func(t *testing.T, f importFixtureParts, before store.AttachmentRef) *Client
		afterwards          func(t *testing.T, f importFixtureParts, before store.AttachmentRef)
		kept                bool
		wantFilename        string
		wantFailures        int
		// wantUnavailable counts the kept rows whose source was unreadable this run.
		wantUnavailable int
		// wantSkipped counts the kept rows this run's policy skipped before the archived bytes were restored.
		wantSkipped int
	}{
		{name: "guid, filename changed", update: `filename = '~/Library/Messages/Attachments/renamed.caf'`, kept: true, wantUnavailable: 2},
		{name: "guid, transfer name changed", update: `transfer_name = 'renamed.caf'`, kept: true, wantFilename: "renamed.caf", wantUnavailable: 2},
		{name: "guid, filename moved outside the root", update: `filename = '../outside.caf'`, kept: true, wantSkipped: 2},
		{
			name:   "guid, declared size over the cap",
			policy: attachmentpolicy.Policy{MaxBytes: 100}, update: `total_bytes = 1000`, kept: true, wantSkipped: 2,
		},
		{name: "guid replaced", update: `guid = 'synthetic-replacement'`},
		{name: "no guid, filename changed", setup: `UPDATE attachment SET guid = NULL`, update: `filename = '~/Library/Messages/Attachments/renamed.caf'`},
		{name: "no guid, declared size changed", setup: `UPDATE attachment SET guid = NULL`, update: `total_bytes = 14`, kept: true, wantUnavailable: 2},
		{
			// The chat grows past the participant limit after its media was archived.
			name:   "policy now skips it",
			policy: attachmentpolicy.Policy{MaxParticipants: 5},
			reimport: func(t *testing.T, f importFixtureParts, _ store.AttachmentRef) *Client {
				t.Helper()
				_, err := f.db.Exec(`INSERT INTO handle (id) VALUES ('h2'),('h3'),('h4'),('h5'),('h6');
					INSERT INTO chat_handle_join VALUES (1,2),(1,3),(1,4),(1,5),(1,6)`)
				require.NoError(t, err)
				return f.c
			},
			kept:        true,
			wantSkipped: 2,
		},
		{
			name: "metadata-only reimport",
			reimport: func(t *testing.T, f importFixtureParts, before store.AttachmentRef) *Client {
				t.Helper()
				removeSource(t, f, before)
				return metadataOnly(t, f)
			},
			kept: true,
		},
		{
			name: "metadata-only reimport of a source without attachment tables",
			reimport: func(t *testing.T, f importFixtureParts, _ store.AttachmentRef) *Client {
				t.Helper()
				_, err := f.db.Exec(`DROP TABLE attachment; DROP TABLE message_attachment_join`)
				require.NoError(t, err)
				return metadataOnly(t, f)
			},
			kept: true,
		},
		{
			name: "destination refuses the rewritten bytes",
			reimport: func(t *testing.T, f importFixtureParts, _ store.AttachmentRef) *Client {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(f.root, "Attachments", "voice.caf"), []byte("caffSYNTHETIX"), 0600))
				// A file where the new blob's directory belongs refuses the write.
				digest := sha256.Sum256([]byte("caffSYNTHETIX"))
				blocker := filepath.Join(f.dest, hex.EncodeToString(digest[:])[:2])
				require.NoError(t, os.WriteFile(blocker, []byte("synthetic destination blocker"), 0600))
				return f.c
			},
			kept:         true,
			wantFailures: 2,
		},
		{
			name: "blob missing and source unreadable",
			reimport: func(t *testing.T, f importFixtureParts, before store.AttachmentRef) *Client {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(f.dest, before.StoragePath)))
				removeSource(t, f, before)
				limited, err := NewClient(f.path, WithAttachments(f.dest, f.root, attachmentpolicy.Policy{}), WithLimit(1))
				require.NoError(t, err)
				t.Cleanup(func() { _ = limited.Close() })
				return limited
			},
			afterwards: func(t *testing.T, f importFixtureParts, before store.AttachmentRef) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(f.root, "Attachments", "voice.caf"), []byte("caffSYNTHETIC"), 0600))
				summary, err := f.c.Import(t.Context(), f.st, f.source)
				require.NoError(t, err)
				for _, message := range []string{"1", "2"} {
					_, after := importedAttachment(t, f.st, f.source, message)
					assert.Equal(t, attachmentpolicy.StateStored, after.State)
					assert.Equal(t, before.ContentHash, after.ContentHash)
					assert.Equal(t, before.StoragePath, after.StoragePath)
				}
				assert.Zero(t, summary.AttachmentsUnavailable)
				assert.FileExists(t, filepath.Join(f.dest, before.StoragePath), "the returned source restores the missing blob")
			},
			kept:            true,
			wantUnavailable: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newImportFixture(t, tc.policy)
			if tc.setup != "" {
				_, err := f.db.Exec(tc.setup)
				require.NoError(err)
			}
			_, err := f.c.Import(t.Context(), f.st, f.source)
			require.NoError(err)
			_, before := importedAttachment(t, f.st, f.source, "1")
			require.Equal(attachmentpolicy.StateStored, before.State)
			require.NotEmpty(before.ContentHash)
			if tc.update != "" {
				_, err = f.db.Exec(`UPDATE attachment SET ` + tc.update)
				require.NoError(err)
			}
			reimport := tc.reimport
			if reimport == nil {
				reimport = removeSource
			}
			summary, err := reimport(t, f, before).Import(t.Context(), f.st, f.source)
			if tc.wantFailures > 0 {
				require.ErrorIs(err, ErrAttachmentImport)
				assert.Equal(tc.wantFailures, summary.AttachmentFailures)
			} else {
				require.NoError(err)
			}
			id, after := importedAttachment(t, f.st, f.source, "1")
			if tc.wantFilename != "" {
				assert.Equal(tc.wantFilename, after.Filename)
			}
			if !tc.kept {
				assert.Equal(attachmentpolicy.StateUnavailable, after.State)
				assert.Equal(attachmentpolicy.SkipSourceUnavailable, after.SkipReason)
				assert.Empty(after.ContentHash)
				return
			}
			assert.Equal(attachmentpolicy.StateStored, after.State)
			assert.Empty(after.SkipReason)
			assert.Equal(before.ContentHash, after.ContentHash)
			assert.Equal(before.StoragePath, after.StoragePath)
			assert.Equal(tc.wantUnavailable, summary.AttachmentsUnavailable)
			assert.Equal(tc.wantSkipped, summary.AttachmentsSkipped)
			var hasAttachments bool
			require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT has_attachments FROM messages WHERE id = ?`), id).Scan(&hasAttachments))
			assert.True(hasAttachments)
			if tc.afterwards != nil {
				tc.afterwards(t, f, before)
			}
		})
	}
}

func TestNewClientRejectsUnusableAttachmentsRoot(t *testing.T) {
	path, _ := testutil.NewIMessageChatDB(t)
	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, nil, 0600))
	for _, root := range []string{filepath.Join(t.TempDir(), "missing"), file} {
		_, err := NewClient(path, WithAttachments(t.TempDir(), root, attachmentpolicy.Policy{}))
		require.ErrorContains(t, err, "messages root")
	}
}

func TestImportAttachmentFilenameFromTransferName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
	for name, want := range map[string]string{"../../voice.caf": "voice.caf", "..": "attachment"} {
		_, err := db.Exec(`UPDATE attachment SET transfer_name = ?`, name)
		require.NoError(err)
		_, err = c.Import(t.Context(), st, source)
		require.NoError(err)
		_, ref := importedAttachment(t, st, source, "1")
		assert.Equal(want, ref.Filename, name)
	}
}

func TestImportCapturesFromDefaultMessagesRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves the home directory from USERPROFILE")
	}
	require := require.New(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	attachments := filepath.Join(home, "Library", "Messages", "Attachments")
	require.NoError(os.MkdirAll(attachments, 0700))
	require.NoError(os.WriteFile(filepath.Join(attachments, "voice.caf"), []byte("caffSYNTHETIC"), 0600))
	path, _ := testutil.NewIMessageAttachmentChatDB(t)
	c, err := NewClient(path, WithAttachments(t.TempDir(), "", attachmentpolicy.Policy{}))
	require.NoError(err)
	t.Cleanup(func() { _ = c.Close() })
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	_, err = c.Import(t.Context(), st, src.ID)
	require.NoError(err)
	_, ref := importedAttachment(t, st, src.ID, "1")
	assert.Equal(t, attachmentpolicy.StateStored, ref.State)
}

func TestChatParticipantCountIsReadOncePerRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImportFixture(t, attachmentpolicy.Policy{})
	c, db := f.c, f.db
	require.NoError(c.prepareAttachments(t.Context()))
	convID, err := f.st.EnsureConversationWithType(f.source, "synthetic", "group_chat", "Synthetic")
	require.NoError(err)
	first, err := c.chatParticipantCount(t.Context(), f.st, convID, 1)
	require.NoError(err)

	_, err = db.Exec(`INSERT INTO handle (id) VALUES ('h2'),('h3'),('h4');
		INSERT INTO chat_handle_join VALUES (1,2),(1,3),(1,4)`)
	require.NoError(err)
	cached, err := c.chatParticipantCount(t.Context(), f.st, convID, 1)
	require.NoError(err)
	assert.Equal(first, cached, "the chat's size is read once for the run")

	require.NoError(c.prepareAttachments(t.Context()))
	fresh, err := c.chatParticipantCount(t.Context(), f.st, convID, 1)
	require.NoError(err)
	assert.Equal(first+3, fresh, "a new run reads the roster again")
}

func TestImportVanishedJoinRowKeepsStoredOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	_, stored := importedAttachment(t, st, source, "1")
	_, err = db.Exec(`DELETE FROM message_attachment_join`)
	require.NoError(err)
	require.NoError(os.RemoveAll(dest))

	for range 2 {
		summary, err := c.Import(t.Context(), st, source)
		require.NoError(err)
		_, ref := importedAttachment(t, st, source, "1")
		assert.Equal(attachmentpolicy.StateStored, ref.State)
		assert.Empty(ref.SkipReason)
		assert.Equal(stored.StoragePath, ref.StoragePath)
		assert.Equal(stored.ContentHash, ref.ContentHash)
		assert.Zero(summary.AttachmentsUnavailable)
	}
}

func TestImportSummarizesAttachmentFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	for i := range 12 {
		_, err := db.Exec(`INSERT INTO message (date,text,cache_has_attachments) VALUES (?, 'More', 1)`, 700000100+i)
		require.NoError(err)
		_, err = db.Exec(`INSERT INTO chat_message_join VALUES (1, ?)`, 3+i)
		require.NoError(err)
		_, err = db.Exec(`INSERT INTO message_attachment_join VALUES (?, 1)`, 3+i)
		require.NoError(err)
	}
	require.NoError(os.RemoveAll(dest))
	require.NoError(os.WriteFile(dest, []byte("synthetic destination blocker"), 0600))
	c.pageSize = 5
	c.limit = 13

	summary, err := c.Import(t.Context(), st, source)
	require.ErrorIs(err, ErrAttachmentImport)
	assert.Equal(13, summary.AttachmentFailures)
	assert.Equal(13, summary.MessagesImported)
	assert.Zero(summary.Skipped)
	require.EqualError(err, "13 messages had attachment errors: "+ErrAttachmentImport.Error())
	var messages int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source).Scan(&messages))
	assert.Equal(13, messages, "a persisted message with a failed attachment must consume the limit")
	for _, message := range []string{"1", "2"} {
		_, ref := importedAttachment(t, st, source, message)
		assert.Equal(attachmentpolicy.StateFailed, ref.State)
		assert.Equal(attachmentpolicy.SkipFetchFailure, ref.SkipReason)
		assert.Empty(ref.ContentHash)
	}
}

func TestImportRewrittenSourceOfSameLength(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, _, st, source, root, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := c.Import(t.Context(), st, source)
	require.NoError(err)
	_, before := importedAttachment(t, st, source, "1")
	require.Equal(attachmentpolicy.StateStored, before.State)

	require.NoError(os.WriteFile(filepath.Join(root, "Attachments", "voice.caf"), []byte("caffSYNTHETIX"), 0600))
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, after := importedAttachment(t, st, source, "1")
	assert.NotEqual(before.ContentHash, after.ContentHash)
	contents, err := os.ReadFile(filepath.Join(dest, after.StoragePath))
	require.NoError(err)
	assert.Equal("caffSYNTHETIX", string(contents))
}

// One invalid byte in a source name must not fail the message's attachment import on every run.
func TestImportAttachmentWithInvalidUTF8Names(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, db, st, source, _, dest := attachmentFixture(t, attachmentpolicy.Policy{})
	_, err := db.Exec(`UPDATE attachment SET guid = ?, filename = ?, transfer_name = ?, mime_type = ?, uti = ?`,
		"guid\xfe", "~/Library/Messages/Attachments/voice.caf", "transfer\xfd.caf", "audio/x-caf\xfc", "uti\xfb")
	require.NoError(err)
	_, err = c.Import(t.Context(), st, source)
	require.NoError(err)
	_, ref := importedAttachment(t, st, source, "1")
	assert.Equal(attachmentpolicy.StateStored, ref.State)
	b, err := os.ReadFile(filepath.Join(dest, ref.StoragePath))
	require.NoError(err)
	assert.Equal("caffSYNTHETIC", string(b))
	var metadata map[string]any
	require.NoError(json.Unmarshal([]byte(ref.Metadata), &metadata))
	assert.Equal("guid\uFFFD", metadata["guid"])
	assert.Equal("uti\uFFFD", metadata["uti"])
	// PostgreSQL rejects invalid UTF-8 in text columns, so the stored columns must be clean too.
	assert.Equal("transfer\uFFFD.caf", ref.Filename)
	assert.Equal("audio/x-caf\uFFFD", ref.MimeType)
}

// The Files view groups by MIME type alone, so an audio attachment whose chat.db row has no
// mime_type must still carry one or the Audio filter omits it.
func TestImportAudioUTIWithoutMIMETypeIsClassifiedAudio(t *testing.T) {
	for _, tc := range []struct{ uti, want string }{
		{"com.apple.coreaudio-format", "audio/x-caf"},
		{"public.mp3", "audio/mpeg"},
		{"public.mpeg-4-audio", "audio/mp4"},
		{"public.aiff-audio", "audio/x-aiff"},
		{"com.microsoft.waveform-audio", "audio/x-wav"},
	} {
		t.Run(tc.uti, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, db, st, source, _, _ := attachmentFixture(t, attachmentpolicy.Policy{})
			_, err := db.Exec(`UPDATE attachment SET mime_type = NULL, uti = ?`, tc.uti)
			require.NoError(err)
			_, err = c.Import(t.Context(), st, source)
			require.NoError(err)
			_, ref := importedAttachment(t, st, source, "1")
			assert.Equal("audio", ref.MediaType)
			assert.Equal(tc.want, ref.MimeType)
			var metadata map[string]any
			require.NoError(json.Unmarshal([]byte(ref.Metadata), &metadata))
			assert.Empty(metadata["mime_type"], "metadata keeps what chat.db said")
		})
	}
}

// A metadata-only run never attempts a file, so the media policy has nothing to skip.
func TestImportMetadataOnlyDoesNotCountPolicySkips(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImportFixture(t, attachmentpolicy.Policy{})
	c, err := NewClient(f.path, WithAttachments("", "", attachmentpolicy.Policy{MaxParticipants: 1}))
	require.NoError(err)
	t.Cleanup(func() { _ = c.Close() })
	summary, err := c.Import(t.Context(), f.st, f.source)
	require.NoError(err)
	assert.Zero(summary.AttachmentsSkipped)
	_, ref := importedAttachment(t, f.st, f.source, "1")
	assert.Equal(attachmentpolicy.StatePending, ref.State)
}

func conversationMemberCount(t *testing.T, f importFixtureParts) int {
	t.Helper()
	var metadata string
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`SELECT COALESCE(CAST(metadata AS TEXT), '') FROM conversations
		WHERE source_id = ?`), f.source).Scan(&metadata))
	require.NotEmpty(t, metadata, "the conversation has no member_count")
	var record struct {
		MemberCount int `json:"member_count"`
	}
	require.NoError(t, json.Unmarshal([]byte(metadata), &record))
	return record.MemberCount
}

// Purge trusts the stored count, so a run must refresh it even when no attachment is read.
func TestImportRefreshesMemberCountWithoutAttachments(t *testing.T) {
	require := require.New(t)
	f := newImportFixture(t, attachmentpolicy.Policy{})
	_, err := f.c.Import(t.Context(), f.st, f.source)
	require.NoError(err)
	require.Equal(2, conversationMemberCount(t, f))

	_, err = f.db.Exec(`INSERT INTO handle (id) VALUES ('h2'),('h3'),('h4'),('h5'),('h6'),('h7'),('h8'),('h9'),('h10'),('h11'),
		('h12'),('h13'),('h14'),('h15'),('h16'),('h17'),('h18'),('h19'),('h20');
		INSERT INTO chat_handle_join SELECT 1, ROWID FROM handle WHERE ROWID > 1`)
	require.NoError(err)
	_, err = f.c.Import(t.Context(), f.st, f.source)
	require.NoError(err)
	require.Equal(21, conversationMemberCount(t, f))

	_, err = f.db.Exec(`DELETE FROM chat_handle_join WHERE handle_id > 1; DELETE FROM message_attachment_join`)
	require.NoError(err)
	_, err = f.c.Import(t.Context(), f.st, f.source)
	require.NoError(err)
	require.Equal(2, conversationMemberCount(t, f), "a chat without joins still has its roster refreshed")
}

func TestImportHasAttachmentsFollowsSourceFlagWithoutRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f importFixtureParts) *Client
	}{
		{"no join rows", func(t *testing.T, f importFixtureParts) *Client {
			t.Helper()
			_, err := f.db.Exec(`DELETE FROM message_attachment_join`)
			require.NoError(t, err)
			return f.c
		}},
		{"no attachment tables", func(t *testing.T, f importFixtureParts) *Client {
			t.Helper()
			_, err := f.db.Exec(`DROP TABLE attachment; DROP TABLE message_attachment_join`)
			require.NoError(t, err)
			c, err := NewClient(f.path, WithAttachments("", "", attachmentpolicy.Policy{}))
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			f := newImportFixture(t, attachmentpolicy.Policy{})
			c := tc.setup(t, f)
			hasAttachments := func() bool {
				var flag bool
				require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT has_attachments FROM messages
					WHERE source_id = ? AND source_message_id = '1'`), f.source).Scan(&flag))
				return flag
			}
			for _, flag := range []int{0, 1, 0, 1} {
				_, err := f.db.Exec(`UPDATE message SET cache_has_attachments = ?`, flag)
				require.NoError(err)
				_, err = c.Import(t.Context(), f.st, f.source)
				require.NoError(err)
				require.Equal(flag == 1, hasAttachments(), "source flag %d", flag)
			}
		})
	}
}

func TestImportUnchangedFlagWithoutRowsWritesNoExtraUpdate(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger counts statements")
	require := require.New(t)
	f := newImportFixture(t, attachmentpolicy.Policy{})
	_, err := f.db.Exec(`DELETE FROM message_attachment_join; UPDATE message SET cache_has_attachments = 0`)
	require.NoError(err)
	_, err = f.c.Import(t.Context(), f.st, f.source)
	require.NoError(err)
	_, err = f.st.DB().Exec(`CREATE TABLE synthetic_flag_updates (message_id INTEGER);
		CREATE TRIGGER synthetic_flag_updates AFTER UPDATE OF has_attachments ON messages
		BEGIN INSERT INTO synthetic_flag_updates VALUES (NEW.id); END`)
	require.NoError(err)
	_, err = f.c.Import(t.Context(), f.st, f.source)
	require.NoError(err)
	var updates int
	require.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM synthetic_flag_updates`).Scan(&updates))
	require.Equal(2, updates, "only the message upsert may write each message")
}
