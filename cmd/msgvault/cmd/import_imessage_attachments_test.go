package cmd

import (
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/testutil"
)

func resetIMessageAttachmentFlags(t *testing.T) {
	t.Helper()
	root, scope, participants, maxMB := importImessageAttachmentsRoot, importImessageMediaScope, importImessageMediaMaxParticipants, importImessageMaxMediaMB
	t.Cleanup(func() {
		importImessageAttachmentsRoot, importImessageMediaScope, importImessageMediaMaxParticipants, importImessageMaxMediaMB = root, scope, participants, maxMB
	})
	importImessageMediaScope, importImessageMediaMaxParticipants, importImessageMaxMediaMB = "all", 50, 250
}

func TestIMessageAttachmentOptionsImport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	resetIMessageAttachmentFlags(t)
	root, dest := t.TempDir(), t.TempDir()
	importImessageAttachmentsRoot = root
	require.NoError(os.WriteFile(filepath.Join(root, "voice.caf"), []byte("caffSYNTHETIC"), 0600))
	require.NoError(os.WriteFile(filepath.Join(root, "note.txt"), []byte("synthetic note"), 0600))
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
 CREATE TABLE message (guid TEXT,text TEXT,attributedBody BLOB,date INTEGER,is_from_me INTEGER,service TEXT,cache_has_attachments INTEGER,handle_id INTEGER);
 CREATE TABLE handle (id TEXT);
 CREATE TABLE chat (guid TEXT,display_name TEXT,chat_identifier TEXT);
 CREATE TABLE chat_message_join (chat_id INTEGER,message_id INTEGER);
 CREATE TABLE chat_handle_join (chat_id INTEGER,handle_id INTEGER);
 CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER,attachment_id INTEGER);
 INSERT INTO message (ROWID,guid,text,attributedBody,date,is_from_me,service,cache_has_attachments,handle_id)
 VALUES (31,'synthetic',NULL,NULL,700000000,0,'iMessage',0,NULL),
        (47,'synthetic-second','Second message',NULL,700000001,0,'SMS',0,NULL);
 INSERT INTO attachment (ROWID,filename) VALUES (14,'~/Library/Messages/Attachments/voice.caf'),(21,'note.txt');
 INSERT INTO message_attachment_join VALUES (31,14),(47,21);
 `)
	require.NoError(err)
	opts, err := buildImessageOpts(slog.Default(), dest)
	require.NoError(err)
	client, err := imessage.NewClient(path, opts...)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)
	summary, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(2, summary.MessagesImported)
	rows, err := st.DB().Query(`SELECT m.source_message_id,a.storage_path,a.attachment_state
		FROM attachments a JOIN messages m ON m.id=a.message_id ORDER BY a.id`)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(rows.Close()) })
	got := map[string]string{}
	count := 0
	for rows.Next() {
		var message, storagePath, state string
		require.NoError(rows.Scan(&message, &storagePath, &state))
		assert.Equal("stored", state)
		b, err := os.ReadFile(filepath.Join(dest, storagePath))
		require.NoError(err)
		got[message] = string(b)
		count++
	}
	require.NoError(rows.Err())
	assert.Equal(2, count)
	assert.Equal(map[string]string{"31": "caffSYNTHETIC", "47": "synthetic note"}, got)
}

func TestIMessageAttachmentOptionsValidate(t *testing.T) {
	resetIMessageAttachmentFlags(t)
	for _, tc := range []struct {
		scope        string
		participants int
		mb           int64
	}{
		{"invalid", 50, 250}, {"all", -1, 250}, {"all", 50, -1}, {"all", 50, 1 << 60},
	} {
		importImessageMediaScope, importImessageMediaMaxParticipants, importImessageMaxMediaMB = tc.scope, tc.participants, tc.mb
		_, err := buildImessageOpts(slog.Default(), t.TempDir())
		require.Error(t, err)
	}
}
