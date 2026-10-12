package imazingcsv

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingNotifyingSelectedAttachmentFailureSuppressesPublication(t *testing.T) {
	for _, boundary := range []string{"database", "storage", "missing"} {
		t.Run(boundary, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-attachment", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
			require.NoError(err)
			row := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "sender@example.net", "Synthetic Sender", "", "", "", "Synthetic ready attachment", "asset.bin", "application/octet-stream"}
			export := newTestExport(t, [][]string{row})
			require.NoError(os.MkdirAll(filepath.Join(export, "attachments"), 0o700))
			switch boundary {
			case "storage", "database":
				require.NoError(os.WriteFile(filepath.Join(export, "attachments", "asset.bin"), []byte("Synthetic attachment bytes"), 0o600))
			}
			storage := t.TempDir()
			selectedStorage := storage
			switch boundary {
			case "storage":
				selectedStorage = filepath.Join(t.TempDir(), "blocked")
				require.NoError(os.WriteFile(selectedStorage, []byte("synthetic non-directory"), 0o600))
			case "database":
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imazing_attachment_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic selected attachment failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_attachment_abort BEFORE INSERT ON attachments FOR EACH ROW EXECUTE FUNCTION synthetic_imazing_attachment_abort()`)
				} else {
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_attachment_abort BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'synthetic selected attachment failure'); END`)
				}
				require.NoError(err)
			}
			_, err = NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true, AttachmentsDir: selectedStorage}).ImportPath(t.Context(), export)
			switch boundary {
			case "database":
				require.ErrorContains(err, "synthetic selected attachment failure")
			case "storage":
				require.ErrorContains(err, "store attachment")
			default:
				require.ErrorContains(err, "selected attachment unavailable")
			}
			for _, table := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "attachments", "import_occurrence_allocations", "import_message_decisions", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				assert.Zero(count, "failed selected attachment must not publish "+table)
			}
			if boundary == "database" {
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_attachment_abort ON attachments`)
					require.NoError(err)
					_, err = st.DB().Exec(`DROP FUNCTION synthetic_imazing_attachment_abort()`)
				} else {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_attachment_abort`)
				}
				require.NoError(err)
			}
			require.NoError(os.WriteFile(filepath.Join(export, "attachments", "asset.bin"), []byte("Synthetic attachment bytes"), 0o600))
			retry, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true, AttachmentsDir: storage}).ImportPath(t.Context(), export)
			require.NoError(err)
			require.NotEmpty(retry.ImportRunID)
			for _, table := range []string{"messages", "message_bodies", "message_raw", "attachments", "import_occurrence_allocations", "import_message_decisions", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				assert.Equal(1, count, "successful ready retry publishes exactly one "+table)
			}
			var decisionRun string
			require.NoError(st.DB().QueryRow("SELECT import_run_id FROM import_message_decisions").Scan(&decisionRun))
			assert.Equal(retry.ImportRunID, decisionRun, "eligibility belongs to the successful ready retry")
		})
	}
}

func TestImportImazingNotifyingAttachmentUpdateFailurePreservesPriorSnapshot(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-attachment", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	row := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "sender@example.net", "Synthetic Sender", "", "", "", "Synthetic snapshot v1", "asset.bin", "application/octet-stream"}
	export := newTestExport(t, [][]string{row})
	require.NoError(os.MkdirAll(filepath.Join(export, "attachments"), 0o700))
	file := filepath.Join(export, "attachments", "asset.bin")
	require.NoError(os.WriteFile(file, []byte("Synthetic v1 bytes"), 0o600))
	storage := t.TempDir()
	first, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: storage}).ImportPath(t.Context(), export)
	require.NoError(err)
	oldAttachment := singleImportedAttachment(t, st)
	var messageID int64
	require.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&messageID))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imazing_attachment_update_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic selected update failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_attachment_update_abort BEFORE UPDATE ON attachments FOR EACH ROW EXECUTE FUNCTION synthetic_imazing_attachment_update_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_attachment_update_abort BEFORE UPDATE ON attachments BEGIN SELECT RAISE(ABORT,'synthetic selected update failure'); END`)
	}
	require.NoError(err)
	row[11] = "Synthetic snapshot v2"
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{row})
	require.NoError(os.WriteFile(file, []byte("Synthetic v2 bytes"), 0o600))
	_, err = NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true, AttachmentsDir: storage}).ImportPath(t.Context(), export)
	require.ErrorContains(err, "synthetic selected update failure")
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), messageID).Scan(&body))
	assert.Equal("Synthetic snapshot v1", body, "required attachment failure must leave the prior body readable")
	after := singleImportedAttachment(t, st)
	assert.Equal(oldAttachment.id, after.id)
	assert.Equal(oldAttachment.contentHash, after.contentHash)
	assert.Equal(oldAttachment.storagePath, after.storagePath)
	var decisionRun string
	require.NoError(st.DB().QueryRow("SELECT import_run_id FROM import_message_decisions").Scan(&decisionRun))
	assert.Equal(first.ImportRunID, decisionRun)
	var events int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
	assert.Zero(events, "updates do not notify an existing silent occurrence")
}
