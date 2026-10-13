package slack

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticSlackdumpMedia(t *testing.T) (string, string) {
	t.Helper()
	root := syntheticImportSlackdump(t)
	Require.NoError(t, os.WriteFile(filepath.Join(root, "example-channel/2024-01-01.json"), []byte(`[{"type":"message","ts":"1704067200.000001","user":"U_PEER","text":"Synthetic offline media","files":[{"id":"F_EXAMPLE","name":"example.txt","mimetype":"text/plain","size":15}]}]`), 0o600))
	path := filepath.Join(root, "example-channel/attachments/F_EXAMPLE-example.txt")
	Require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	Require.NoError(t, os.WriteFile(path, []byte("synthetic bytes"), 0o600))
	return root, path
}

func configureSlackdumpNotifications(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-slackdump", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "slackdump", Kinds: []string{"message"}}}})
	Require.NoError(t, err)
}

func installSlackdumpMediaAbort(t *testing.T, st *store.Store, table, condition string) {
	t.Helper()
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_slackdump_media_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF ` + condition + ` THEN RAISE EXCEPTION 'synthetic required Slackdump media failure'; END IF; RETURN NEW; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_slackdump_media_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_slackdump_media_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_slackdump_media_abort BEFORE INSERT ON ` + table + ` WHEN ` + condition + ` BEGIN SELECT RAISE(ABORT,'synthetic required Slackdump media failure'); END`)
	}
	Require.NoError(t, err)
}

func removeSlackdumpMediaAbort(t *testing.T, st *store.Store, table string) {
	t.Helper()
	query := `DROP TRIGGER synthetic_slackdump_media_abort`
	if st.IsPostgreSQL() {
		query += ` ON ` + table
	}
	_, err := st.DB().Exec(query)
	Require.NoError(t, err)
}

func TestImportSlackdumpNotifyingSelectedMediaFailureRollsBackAndRetries(t *testing.T) {
	for _, failure := range []string{"missing", "storage", "attachments", "mcp_event_log"} {
		t.Run(failure, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			path, mediaPath := syntheticSlackdumpMedia(t)
			st := testutil.NewTestStore(t)
			configureSlackdumpNotifications(t, st)
			storage := filepath.Join(t.TempDir(), "media")
			switch failure {
			case "missing":
				require.NoError(os.Remove(mediaPath))
			case "storage":
				require.NoError(os.WriteFile(storage, []byte("synthetic blocker"), 0o600))
			default:
				installSlackdumpMediaAbort(t, st, failure, "TRUE")
			}
			summary, err := ImportSlackdump(t.Context(), st, path, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true, AttachmentsDir: storage})
			require.Error(err)
			assert.Zero(summary.AttachmentsDownloaded)
			for _, table := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "attachments", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
				assert.Zero(n, "failed required selected media leaked "+table)
			}
			switch failure {
			case "missing":
				require.NoError(os.WriteFile(mediaPath, []byte("synthetic bytes"), 0o600))
			case "storage":
				require.NoError(os.Remove(storage))
			default:
				removeSlackdumpMediaAbort(t, st, failure)
			}
			summary, err = ImportSlackdump(t.Context(), st, path, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true, AttachmentsDir: storage})
			require.NoError(err)
			assert.Equal(1, summary.AttachmentsDownloaded)
			var n int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&n))
			assert.Equal(1, n)
		})
	}
}

func TestImportSlackdumpNotifyingMediaReadyBeforeJournal(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path, _ := syntheticSlackdumpMedia(t)
	st := testutil.NewTestStore(t)
	configureSlackdumpNotifications(t, st)
	storage := t.TempDir()
	installSlackdumpMediaAbort(t, st, "mcp_event_log", `NOT EXISTS (SELECT 1 FROM attachments WHERE source_attachment_id='slack:F_EXAMPLE' AND attachment_state='stored' AND content_hash<>'' AND storage_path<>'') OR NOT EXISTS (SELECT 1 FROM messages WHERE has_attachments=TRUE AND attachment_count=1)`)
	summary, err := ImportSlackdump(t.Context(), st, path, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true, AttachmentsDir: storage})
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsDownloaded)
	var storagePath, hash, state string
	require.NoError(st.DB().QueryRow(`SELECT storage_path,content_hash,attachment_state FROM attachments WHERE source_attachment_id='slack:F_EXAMPLE'`).Scan(&storagePath, &hash, &state))
	assert.Equal(string(attachmentpolicy.StateStored), state)
	assert.NotEmpty(hash)
	content, err := os.ReadFile(filepath.Join(storage, filepath.FromSlash(storagePath)))
	require.NoError(err)
	assert.Equal("synthetic bytes", string(content))
}
