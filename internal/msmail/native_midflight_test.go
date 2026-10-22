package msmail

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMicrosoftCaptureUsesCurrentImmutableFolder(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: SourceType, Kinds: []string{"message"}}, {Family: "msgvault.source_message_archived", SourceType: SourceType, Kinds: []string{"message"}}, {Family: "msgvault.inbox_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	graph := newFakeGraph(t)
	var move bool
	graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" {
			graph.writeJSON(w, map[string]any{"id": "synthetic-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
			return
		}
		if move && strings.HasSuffix(r.URL.Path, "/$value") {
			move = false
			graph.put("synthetic-moving-arrival", "archive")
		}
		graph.serve(w, r)
	})
	c := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) { return []string{"Mail.Read"}, nil }))
	_, err = Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
	require.NoError(err)
	graph.put("synthetic-moving-arrival", "inbox")
	move = true
	_, err = Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
	require.NoError(err)
	var folder string
	require.NoError(st.DB().QueryRow(`SELECT n.provider_id FROM native_message_scopes n WHERE EXISTS (SELECT 1 FROM mcp_event_log e WHERE e.family='msgvault.inbox_message_archived' AND e.scope_id=n.id)`).Scan(&folder))
	assert.Equal("archive", folder, "the current immutable message snapshot must win over a stale delta folder")
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count)
}

func TestNativeMicrosoftMidSyncDenialRetiresAuthority(t *testing.T) {
	for _, route := range []string{"inventory", "mime"} {
		for _, status := range []int{400, 401, 403, 404} {
			t.Run(route+"/"+http.StatusText(status), func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				st := testutil.NewTestStore(t)
				enableNativeMailEvents(t, st)
				graph := newFakeGraph(t)
				var failure bool
				graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/me" {
						graph.writeJSON(w, map[string]any{"id": "synthetic-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
						return
					}
					if failure && (route == "inventory" && r.URL.Path == "/me/mailFolders" || route == "mime" && strings.HasSuffix(r.URL.Path, "/$value")) {
						w.WriteHeader(status)
						return
					}
					graph.serve(w, r)
				})
				c := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) { return []string{"Mail.Read"}, nil }))
				_, err := Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
				require.NoError(err)
				source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
				require.NoError(err)
				require.NotNil(source)
				before, err := st.GetMCPResource(t.Context(), "source", source.ID)
				require.NoError(err)
				failure = true
				graph.put("synthetic-denied-arrival", "inbox")
				_, _ = Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
				allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
				require.NoError(err)
				denied := status == 401 || status == 403
				assert.Equal(!denied, allowed)
				after, err := st.GetMCPResource(t.Context(), "source", source.ID)
				require.NoError(err)
				if denied {
					assert.NotEqual(before.Reference, after.Reference)
				} else {
					assert.Equal(before.Reference, after.Reference)
				}
				assert.Zero(nativeMailEventCount(t, st))
			})
		}
	}
}
