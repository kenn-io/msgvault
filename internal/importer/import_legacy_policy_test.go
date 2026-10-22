package importer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEMLLegacyCheckpointCannotUpgradeToNotifications(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	root := filepath.Join(t.TempDir(), "Synthetic.mailbox")
	require.NoError(os.MkdirAll(root, 0o700))
	raw := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <synthetic-legacy-policy@example.net>\r\nSubject: Synthetic legacy policy\r\n\r\nSynthetic body\r\n")
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), raw, 0o600))
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-legacy-policy", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
	require.NoError(err)
	source, err := st.GetOrCreateSource("eml", "owner@example.net")
	require.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
	legacyID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
	require.NoError(err)
	// Use the real native checkpoint schema without any new import provenance.
	cursor, err := json.Marshal(emlCheckpoint{RootDir: root, MailboxIndex: 0, MailboxPath: root})
	require.NoError(err)
	require.NoError(st.ScopedToSync(source.ID, legacyID).UpdateSyncCheckpointContext(t.Context(), legacyID, &store.Checkpoint{PageToken: string(cursor)}))
	require.NoError(execution.Release())
	_, err = ImportEMLDir(t.Context(), st, root, EMLImportOptions{Identifier: "owner@example.net", NotifyImports: true})
	require.Error(err, "a pre-provenance checkpoint cannot acquire notification policy during recovery")
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Zero(count)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
	// The supported explicit fresh-run switch changes policy without pretending
	// to recover a silent historical operation.
	summary, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{Identifier: "owner@example.net", NotifyImports: true, NoResume: true})
	require.NoError(err)
	require.NotNil(summary)
	assert.False(summary.WasResumed)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
	assert.Equal(1, count)
}
