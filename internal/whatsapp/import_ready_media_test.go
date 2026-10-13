package whatsapp

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticWhatsAppMediaImport(t *testing.T) (string, ImportOptions) {
	t.Helper()
	path := createGroupParticipantsImportFixture(t, func(db *sql.DB) {
		_, err := db.Exec(`DELETE FROM message WHERE _id=200; DELETE FROM chat WHERE _id=20;
INSERT INTO message_media VALUES(100,'image/example','synthetic caption',0,'image.bin',128,64,7)`)
		Require.NoError(t, err)
	})
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.NotifyImports = true
	opts.MediaDir, opts.AttachmentsDir = t.TempDir(), t.TempDir()
	Require.NoError(t, os.WriteFile(filepath.Join(opts.MediaDir, "image.bin"), []byte("synthetic media bytes"), 0o600))
	return path, opts
}

func abortSyntheticWhatsAppInsert(t *testing.T, st *store.Store, table string) {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_whatsapp_media_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required media failure'; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_whatsapp_media_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_whatsapp_media_abort()`)
		Require.NoError(t, err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_whatsapp_media_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic required media failure'); END`)
		Require.NoError(t, err)
	}
}

func TestImportWhatsAppAndroidSelectedMediaFailuresRollBackAndRetry(t *testing.T) {
	for _, failure := range []string{"missing-file", "storage-failure", "attachment-sql", "journal-sql"} {
		t.Run(failure, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			path, opts := syntheticWhatsAppMediaImport(t)
			st := testutil.NewTestStore(t)
			configureSyntheticWhatsAppImports(t, st, true)
			originalRoot := opts.AttachmentsDir
			switch failure {
			case "missing-file":
				require.NoError(os.Remove(filepath.Join(opts.MediaDir, "image.bin")))
			case "storage-failure":
				opts.AttachmentsDir = filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(os.WriteFile(opts.AttachmentsDir, []byte("synthetic obstruction"), 0o600))
			case "attachment-sql":
				abortSyntheticWhatsAppInsert(t, st, "attachments")
			case "journal-sql":
				abortSyntheticWhatsAppInsert(t, st, "mcp_event_log")
			}
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err, "native independent-message failure policy continues")
			assert.Zero(summary.MessagesAdded)
			assert.Positive(summary.Errors)
			for _, table := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "attachments", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
				assert.Zero(n, "failed selected media leaked "+table)
			}
			opts.AttachmentsDir = originalRoot
			require.NoError(os.WriteFile(filepath.Join(opts.MediaDir, "image.bin"), []byte("synthetic media bytes"), 0o600))
			if failure == "attachment-sql" || failure == "journal-sql" {
				if st.IsPostgreSQL() {
					table := map[string]string{"attachment-sql": "attachments", "journal-sql": "mcp_event_log"}[failure]
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_whatsapp_media_abort ON ` + table)
				} else {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_whatsapp_media_abort`)
				}
				require.NoError(err)
			}
			summary, err = NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err)
			assert.Equal(int64(1), summary.MessagesAdded)
			assert.Zero(summary.Errors)
			var events, refs int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE source_part_key='whatsapp:media' AND attachment_state='stored'`).Scan(&refs))
			assert.Equal(1, events)
			assert.Equal(1, refs)
		})
	}
}

func TestImportWhatsAppAndroidReadyMediaPrecedesJournalAndRetainsStoredOccurrence(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path, opts := syntheticWhatsAppMediaImport(t)
	st := testutil.NewTestStore(t)
	configureSyntheticWhatsAppImports(t, st, true)
	// The real journal insert must already see the source's full ready reference.
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_whatsapp_media_ready() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NOT EXISTS(SELECT 1 FROM attachments WHERE source_part_key='whatsapp:media' AND attachment_state='stored' AND storage_path<>'' AND content_hash<>'' AND width=128 AND height=64 AND duration_ms=7000) THEN RAISE EXCEPTION 'native media not ready before journal'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_whatsapp_media_ready BEFORE INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION synthetic_whatsapp_media_ready()`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_whatsapp_media_ready BEFORE INSERT ON mcp_event_log WHEN NOT EXISTS(SELECT 1 FROM attachments WHERE source_part_key='whatsapp:media' AND attachment_state='stored' AND storage_path<>'' AND content_hash<>'' AND width=128 AND height=64 AND duration_ms=7000) BEGIN SELECT RAISE(ABORT,'native media not ready before journal'); END`)
		require.NoError(err)
	}
	summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.NoError(err)
	assert.Zero(summary.Errors)
	assert.Equal(int64(1), summary.MessagesAdded)
	var storedPath, hash string
	var width, height, duration int64
	require.NoError(st.DB().QueryRow(`SELECT storage_path,content_hash,width,height,duration_ms FROM attachments WHERE source_part_key='whatsapp:media'`).Scan(&storedPath, &hash, &width, &height, &duration))
	content, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, storedPath))
	require.NoError(err)
	assert.Equal("synthetic media bytes", string(content))
	require.NotEmpty(hash)
	assert.Equal(int64(128), width)
	assert.Equal(int64(64), height)
	assert.Equal(int64(7000), duration)
	require.NoError(os.Remove(filepath.Join(opts.MediaDir, "image.bin")))
	summary, err = NewImporter(st, nil).Import(t.Context(), path, opts)
	require.NoError(err)
	assert.Zero(summary.Errors, "failed reacquisition retains a stronger complete reference")
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
	var retainedPath, retainedHash string
	require.NoError(st.DB().QueryRow(`SELECT storage_path,content_hash,width,height,duration_ms FROM attachments WHERE source_part_key='whatsapp:media'`).Scan(&retainedPath, &retainedHash, &width, &height, &duration))
	assert.Equal(storedPath, retainedPath)
	assert.Equal(hash, retainedHash)
	assert.Equal(int64(128), width)
	assert.Equal(int64(64), height)
	assert.Equal(int64(7000), duration)
}

func TestImportWhatsAppAndroidExplicitMediaPolicySkipsAreReady(t *testing.T) {
	for _, reason := range []string{"unselected", "oversize"} {
		t.Run(reason, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			path, opts := syntheticWhatsAppMediaImport(t)
			if reason == "unselected" {
				opts.MediaDir, opts.AttachmentsDir = "", ""
			} else {
				opts.MaxMediaFileSize = 1
			}
			st := testutil.NewTestStore(t)
			configureSyntheticWhatsAppImports(t, st, true)
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err)
			assert.Zero(summary.Errors)
			var state, skip string
			require.NoError(st.DB().QueryRow(`SELECT attachment_state,attachment_skip_reason FROM attachments WHERE source_part_key='whatsapp:media'`).Scan(&state, &skip))
			assert.Equal("skipped", state)
			assert.Equal(map[string]string{"unselected": "policy_scope", "oversize": "size_cap"}[reason], skip)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			assert.Equal(1, events)
		})
	}
}
