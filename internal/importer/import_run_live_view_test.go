package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEMLDirCannotInheritLiveAuthority(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st, tmp := testutil.NewTestStore(t), t.TempDir()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-import", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	root := filepath.Join(tmp, "Synthetic.mailbox")
	require.NoError(os.MkdirAll(root, 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <offline-context@example.net>\r\nSubject: Synthetic offline context\r\n\r\nSynthetic body\r\n"), 0o600))
	live := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	summary, err := ImportEMLDir(t.Context(), live, root, EMLImportOptions{Identifier: "owner@example.net", SourceType: "gmail", NoResume: true})
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&count))
	assert.Zero(count, "native file import cannot inherit a caller's live mode or manufacture Gmail provenance")
}
