package importer

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(err)
	resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
	live := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	require.NoError(IngestRawMessage(t.Context(), live, source.ID, source.Identifier, "", nil,
		"synthetic-live-before-eml", "synthetic-live-before-eml-hash",
		[]byte("From: sender@example.net\r\nTo: owner@example.net\r\nSubject: Synthetic live mail\r\nMessage-ID: <live-before-eml@example.net>\r\n\r\nSynthetic live body"),
		time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC), slog.Default()))
	var liveBefore int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&liveBefore))
	require.Equal(1, liveBefore, "the authorized live Gmail write proves the event capability is active")
	root := filepath.Join(tmp, "Synthetic.mailbox")
	require.NoError(os.MkdirAll(root, 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <offline-context@example.net>\r\nSubject: Synthetic offline context\r\n\r\nSynthetic body\r\n"), 0o600))
	summary, err := ImportEMLDir(t.Context(), live, root, EMLImportOptions{Identifier: "owner@example.net", SourceType: "gmail", NoResume: true})
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&count))
	assert.Equal(liveBefore, count, "native file import cannot inherit live mode or manufacture Gmail provenance")
}
