package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	gmailSync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type authorizedImportGmailAPI struct{ *gmail.MockAPI }

func (authorizedImportGmailAPI) NativeMessageReadPermission() (bool, bool) { return true, true }

func TestImportEMLDirCannotInheritLiveAuthority(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st, tmp := testutil.NewTestStore(t), t.TempDir()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-import", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	const identifier = "owner@example.net"
	liveAPI := gmail.NewMockAPI()
	liveAPI.Profile = &gmail.Profile{EmailAddress: identifier, HistoryID: 1001}
	liveAPI.HistoryID = 1001
	liveAPI.HistoryRecords = []gmail.HistoryRecord{{MessagesAdded: []gmail.HistoryMessage{{Message: gmail.MessageID{ID: "synthetic-live-gmail"}}}}}
	liveAPI.AddMessage("synthetic-live-gmail", testemail.NewMessage().
		From("sender@example.net").
		To(identifier).
		Body("Synthetic live Gmail positive control").
		Bytes(), []string{"INBOX"})
	source, err := st.GetOrCreateSource("gmail", identifier)
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncCursor(source.ID, "1000"))
	source, err = st.GetSourceByID(source.ID)
	require.NoError(err)
	liveSync := gmailSync.New(authorizedImportGmailAPI{MockAPI: liveAPI}, st, nil)
	_, err = liveSync.Incremental(t.Context(), source)
	require.NoError(err, "authorized live Gmail sync should pass the fixture native permission guard")
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&count))
	require.Equal(1, count, "the same Events authorization must capture a normal live Gmail write")
	root := filepath.Join(tmp, "Synthetic.mailbox")
	require.NoError(os.MkdirAll(root, 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <offline-context@example.net>\r\nSubject: Synthetic offline context\r\n\r\nSynthetic body\r\n"), 0o600))
	live := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	summary, err := ImportEMLDir(t.Context(), live, root, EMLImportOptions{Identifier: identifier, SourceType: "gmail", NoResume: true})
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&count))
	assert.Equal(1, count, "native file import cannot inherit live mode or manufacture another Gmail arrival")
}
