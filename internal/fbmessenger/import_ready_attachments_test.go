package fbmessenger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticMessengerMedia(t *testing.T, opts ImportOptions) string {
	t.Helper()
	file := filepath.Join(opts.RootDir, "your_activity_across_facebook/messages/inbox/synthetic_THREAD/message_1.json")
	raw, err := os.ReadFile(file)
	Require.NoError(t, err)
	var thread map[string]any
	Require.NoError(t, json.Unmarshal(raw, &thread))
	messages, ok := thread["messages"].([]any)
	Require.True(t, ok)
	message, ok := messages[0].(map[string]any)
	Require.True(t, ok)
	uri := "your_activity_across_facebook/messages/inbox/synthetic_THREAD/files/synthetic.bin"
	message["files"] = []map[string]any{{"uri": uri}}
	raw, err = json.Marshal(thread)
	Require.NoError(t, err)
	Require.NoError(t, os.WriteFile(file, raw, 0600))
	path := filepath.Join(opts.RootDir, filepath.FromSlash(uri))
	Require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	Require.NoError(t, os.WriteFile(path, []byte("Synthetic cached attachment"), 0600))
	return path
}

func TestImportMessengerSelectedAttachmentFailureRollsBackThenRetries(t *testing.T) {
	for _, failure := range []string{"missing-file", "attachment-sql"} {
		t.Run(failure, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureSyntheticMessengerImports(t, st, true)
			opts := syntheticMessengerImport(t)
			opts.NotifyImports = true
			path := syntheticMessengerMedia(t, opts)
			if failure == "missing-file" {
				require.NoError(os.Remove(path))
			} else {
				messengerSnapshotAbort(t, st, "attachments")
			}
			first, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.Zero(first.MessagesAdded)
			assert.True(first.HardErrors)
			for _, name := range []string{"messages", "message_bodies", "message_raw", "message_recipients", "message_labels", "attachments", "reactions", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&n))
				assert.Zero(n, "failed selected attachment leaked "+name)
			}
			if failure == "missing-file" {
				require.NoError(os.WriteFile(path, []byte("Synthetic cached attachment"), 0600))
			} else {
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_messenger_ready_abort ON attachments`)
				} else {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_messenger_ready_abort`)
				}
				require.NoError(err)
			}
			second, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(first.ImportRunID, second.ImportRunID)
			assert.True(second.WasResumed)
			assert.False(second.HardErrors)
			assert.Equal(int64(1), second.MessagesAdded)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			assert.Equal(1, events)
			var stored, hash string
			require.NoError(st.DB().QueryRow(`SELECT storage_path,content_hash FROM attachments`).Scan(&stored, &hash))
			assert.NotEmpty(stored)
			assert.Len(hash, 64)
		})
	}
}

func TestImportMessengerReadyAttachmentExistsBeforeJournalAndSurvivesReacquisitionGap(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticMessengerImports(t, st, true)
	opts := syntheticMessengerImport(t)
	opts.NotifyImports = true
	path := syntheticMessengerMedia(t, opts)
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_messenger_ready_at_append() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.family='msgvault.import_message_archived' AND (NOT EXISTS(SELECT 1 FROM attachments WHERE attachment_state='stored' AND storage_path<>'' AND content_hash<>'') OR NOT EXISTS(SELECT 1 FROM message_bodies) OR NOT EXISTS(SELECT 1 FROM message_raw) OR NOT EXISTS(SELECT 1 FROM message_recipients) OR NOT EXISTS(SELECT 1 FROM reactions)) THEN RAISE EXCEPTION 'synthetic incomplete ready projection'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_messenger_ready_at_append BEFORE INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION synthetic_messenger_ready_at_append()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_messenger_ready_at_append BEFORE INSERT ON mcp_event_log WHEN NEW.family='msgvault.import_message_archived' AND (NOT EXISTS(SELECT 1 FROM attachments WHERE attachment_state='stored' AND storage_path<>'' AND content_hash<>'') OR NOT EXISTS(SELECT 1 FROM message_bodies) OR NOT EXISTS(SELECT 1 FROM message_raw) OR NOT EXISTS(SELECT 1 FROM message_recipients) OR NOT EXISTS(SELECT 1 FROM reactions)) BEGIN SELECT RAISE(ABORT,'synthetic incomplete ready projection'); END`)
	}
	require.NoError(err)
	first, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(first.HardErrors)
	require.Equal(int64(1), first.MessagesAdded)
	var attachmentID int64
	var stored, hash, filename string
	require.NoError(st.DB().QueryRow(`SELECT id,storage_path,content_hash,filename FROM attachments`).Scan(&attachmentID, &stored, &hash, &filename))
	require.NoError(os.Remove(path))
	second, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(second.HardErrors)
	var gotID int64
	var gotStored, gotHash, gotFilename string
	require.NoError(st.DB().QueryRow(`SELECT id,storage_path,content_hash,filename FROM attachments`).Scan(&gotID, &gotStored, &gotHash, &gotFilename))
	assert.Equal(attachmentID, gotID)
	assert.Equal(stored, gotStored)
	assert.Equal(hash, gotHash)
	assert.Equal(filename, gotFilename)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

func TestImportMessengerUnselectedAttachmentsAreExplicitPolicySkips(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticMessengerImports(t, st, true)
	opts := syntheticMessengerImport(t)
	opts.NotifyImports = true
	syntheticMessengerMedia(t, opts)
	opts.AttachmentsDir = ""
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(summary.HardErrors)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Zero(summary.AttachmentsStored)
	var state, reason string
	require.NoError(st.DB().QueryRow(`SELECT attachment_state,attachment_skip_reason FROM attachments`).Scan(&state, &reason))
	assert.Equal("skipped", state)
	assert.Equal("policy_scope", reason)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}
