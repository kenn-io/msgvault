package slack

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
)

// A failed continuation cannot turn the quiet channels or exact roots from an
// earlier complete native inventory into an authoritative deletion.
func TestNativeSlackPartialInventoryPreservesQuietScopes(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	native := nativeSlackInboxWorkspace(t)
	imp, opts := testImporter(t, native)
	enableNativeSlackInboxEvents(t, imp.store)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	before, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	require.Len(before.Resources, 5)
	original := native.serve()
	var fail atomic.Bool
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
	fail.Store(true)
	_, err = imp.Import(t.Context(), opts)
	require.Error(err)
	for _, resource := range before.Resources {
		current, lookupErr := imp.store.GetMCPResource(t.Context(), "inbox", resource.ID)
		require.NoError(lookupErr, "partial native pagination cannot retire %s", resource.ProviderID)
		assert.Equal(resource.Reference, current.Reference)
	}
	fail.Store(false)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	after, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	assert.Len(after.Resources, 5)
	for _, resource := range before.Resources {
		current, lookupErr := imp.store.GetMCPResource(t.Context(), "inbox", resource.ID)
		require.NoError(lookupErr)
		assert.Equal(resource.Reference, current.Reference, "successful recovery keeps previously present native identities")
	}
}
