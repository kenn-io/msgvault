package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativePSTSelectedMediaIsReadyBeforeOccurrence(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureNativePSTImports(t, st, true, true)
	path := filepath.Join(t.TempDir(), "synthetic-media.pst")
	require.NoError(os.WriteFile(path, syntheticPSTMediaFixture, 0o600))
	// Native MIME refs may retain an empty legacy state. Actual stored bytes
	// require a path and content hash; receipt readers accept both states.
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_pst_require_refs() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.family='msgvault.import_message_archived' AND NOT EXISTS (SELECT 1 FROM attachments WHERE COALESCE(attachment_state,'') IN ('','stored') AND storage_path<>'' AND content_hash<>'') THEN RAISE EXCEPTION 'selected PST refs not ready'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_pst_require_refs BEFORE INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION synthetic_pst_require_refs()`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_pst_require_refs BEFORE INSERT ON mcp_event_log WHEN NEW.family='msgvault.import_message_archived' AND NOT EXISTS (SELECT 1 FROM attachments WHERE COALESCE(attachment_state,'') IN ('','stored') AND storage_path<>'' AND content_hash<>'') BEGIN SELECT RAISE(ABORT,'selected PST refs not ready'); END`)
		require.NoError(err)
	}
	dir := t.TempDir()
	result, err := ImportPst(t.Context(), st, path, PstImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: true, AttachmentsDir: dir})
	require.NoError(err)
	require.NotNil(result)
	require.False(result.HardErrors)
	assert.Zero(result.Errors)
	var storagePath string
	require.NoError(st.DB().QueryRow(`SELECT storage_path FROM attachments WHERE COALESCE(attachment_state,'') IN ('','stored') AND storage_path<>'' AND content_hash<>''`).Scan(&storagePath))
	if !filepath.IsAbs(storagePath) {
		storagePath = filepath.Join(dir, storagePath)
	}
	data, err := os.ReadFile(storagePath)
	require.NoError(err)
	assert.Equal([]byte("Synthetic PST attachment"), data)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

func TestNativePSTUnreadySelectedMediaConsumesNoDecision(t *testing.T) {
	for _, failure := range []string{"parser", "storage"} {
		t.Run(failure, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureNativePSTImports(t, st, true, true)
			fixture := syntheticPSTMediaFixture
			if failure == "parser" {
				fixture = syntheticPSTBrokenFixture
			}
			path := filepath.Join(t.TempDir(), "synthetic-media.pst")
			require.NoError(os.WriteFile(path, fixture, 0o600))
			dir := t.TempDir()
			if failure == "storage" {
				dir = filepath.Join(dir, "blocked")
				require.NoError(os.WriteFile(dir, []byte("synthetic storage obstruction"), 0o600))
			}
			result, err := ImportPst(t.Context(), st, path, PstImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: true, AttachmentsDir: dir})
			require.NoError(err)
			require.NotNil(result)
			assert.True(result.HardErrors)
			for _, table := range []string{"participants", "conversations", "messages", "message_bodies", "message_raw", "message_recipients", "conversation_participants", "attachments", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
				assert.Zero(n, "unready selected media must not leave "+table)
			}
		})
	}
}
