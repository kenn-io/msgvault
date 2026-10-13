package slack

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
)

// Current positive loss of private history is authority even if pagination
// fails before the quiet private channel. Public access must stay independent.
func TestNativeSlackRevokedQuietChannelCannotSurvivePartialInventory(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	native := nativeSlackInboxWorkspace(t)
	native.convs = append(native.convs, &fakeConv{ID: "GPRIVATE", Name: "synthetic-private", Kind: "private", Members: []string{"UME", "USENDER"}})
	imp, opts := testImporter(t, native)
	enableNativeSlackInboxEvents(t, imp.store)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	var public, private store.MCPResource
	for _, resource := range page.Resources {
		if resource.ProviderID == "CEVENTS" {
			public = resource
		}
		if resource.ProviderID == "GPRIVATE" {
			private = resource
		}
	}
	require.Positive(public.ID)
	require.Positive(private.ID)
	original := native.serve()
	var fail atomic.Bool
	fail.Store(true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() && (r.URL.Path == "/users.conversations" || r.URL.Path == "/conversations.list") && r.FormValue("cursor") != "" {
			w.Header().Set("X-Oauth-Scopes", native.scopes)
			native.replyErr(w, "internal_error")
			return
		}
		original.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	imp.client.baseURL = proxy.URL
	native.pageSize = 1
	native.scopes = strings.ReplaceAll(native.scopes, "groups:history,", "")
	_, err = imp.Import(t.Context(), opts)
	require.Error(err, "native inventory deliberately fails after its first public channel")
	_, _, err = imp.store.ValidateMCPEventScope(t.Context(), "msgvault.inbox_message_archived", "inbox", private.ID, []string{"message"})
	require.Error(err, "known lost private history must revoke the omitted quiet channel before callback admission")
	current, err := imp.store.GetMCPResource(t.Context(), "inbox", public.ID)
	require.NoError(err)
	assert.Equal(public.Reference, current.Reference)
	_, _, err = imp.store.ValidateMCPEventScope(t.Context(), "msgvault.inbox_message_archived", "inbox", public.ID, []string{"message"})
	require.NoError(err, "the independent public channel retains current native authority")
	fail.Store(false)
	native.scopes += ",groups:history"
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	restored, err := imp.store.GetMCPResource(t.Context(), "inbox", private.ID)
	require.NoError(err)
	assert.NotEqual(private.Reference, restored.Reference, "restoring native private access creates a new channel lifetime")
	current, err = imp.store.GetMCPResource(t.Context(), "inbox", public.ID)
	require.NoError(err)
	assert.Equal(public.Reference, current.Reference, "private restoration does not rotate independent public authority")
}
