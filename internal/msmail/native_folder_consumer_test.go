package msmail

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMicrosoftFolderInventoryAndAtomicMembership(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: SourceType, Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: SourceType, Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: SourceType, Kinds: []string{"message"}},
	}})
	require.NoError(err)
	graph := newFakeGraph(t)
	var folderStatus int
	graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" {
			graph.writeJSON(w, map[string]any{"id": "synthetic-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
			return
		}
		if r.URL.Path == "/me/mailFolders" && folderStatus != 0 {
			w.WriteHeader(folderStatus)
			return
		}
		graph.serve(w, r)
	})
	c := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) { return []string{"Mail.Read"}, nil }))
	run := func() {
		_, err := Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
		Require.NoError(t, err)
	}
	run()
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(err)
	require.NotNil(source)
	page := func() map[string]store.MCPResource {
		p, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: source.ID})
		Require.NoError(t, err)
		out := map[string]store.MCPResource{}
		for _, r := range p.Resources {
			Assert.Equal(t, "msmail_folder", r.ScopeKind)
			out[r.ProviderID] = r
		}
		return out
	}
	first := page()
	require.Len(first, 2)
	graph.names = map[string]string{"inbox": "Renamed Inbox"}
	run()
	assert.Equal(first["inbox"].Reference, page()["inbox"].Reference)
	folderStatus = http.StatusBadRequest
	_, err = Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
	require.Error(err)
	assert.Equal(first["inbox"].Reference, page()["inbox"].Reference, "failed inventory cannot assert absence")
	folderStatus = 0
	graph.put("synthetic-arrival", "inbox")
	run()
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count, "conversation, source and exact folder are separate occurrences")
	var inboxID int64
	require.NoError(st.DB().QueryRow(`SELECT scope_id FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&inboxID))
	assert.Equal(first["inbox"].ID, inboxID)
	var memberships int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM native_message_scope_memberships`).Scan(&memberships))
	assert.Equal(1, memberships)
	graph.put("synthetic-arrival", "archive")
	run()
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count, "an immutable message move is not another arrival")
	require.NoError(st.DB().QueryRow(`SELECT scope_id FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&inboxID))
	assert.Equal(first["inbox"].ID, inboxID, "a later move must not rewrite capture-time membership")
	graph.folders = []string{"archive"}
	run()
	_, err = st.GetMCPResource(t.Context(), "inbox", first["inbox"].ID)
	require.Error(err, "removed native folder is unavailable even while its archive label remains")
	graph.folders = []string{"inbox", "archive"}
	run()
	assert.NotEqual(first["inbox"].Reference, page()["inbox"].Reference)
}
