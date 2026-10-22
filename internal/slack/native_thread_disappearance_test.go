package slack

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
)

// A root disappearing between native search and canonical replies retires only
// its exact channel/root. An ordinary fetch failure asserts no disappearance.
func TestNativeSlackThreadReadDisappearanceKeepsExactParent(t *testing.T) {
	for _, code := range []string{"thread_not_found", "internal_error"} {
		t.Run(code, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			native := nativeSlackInboxWorkspace(t)
			imp, opts := testImporter(t, native)
			now := tsBase.Add(24 * time.Hour)
			imp.now = func() time.Time { return now }
			enableNativeSlackInboxEvents(t, imp.store)
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
			require.NoError(err)
			resources := map[string]store.MCPResource{}
			for _, resource := range page.Resources {
				resources[resource.ProviderID] = resource
			}
			ownRoot, otherRoot := resources["CEVENTS:"+ts(0)], resources["COTHER:"+ts(0)]
			require.Positive(ownRoot.ID)
			require.Positive(otherRoot.ID)
			original := native.serve()
			var observed atomic.Bool
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/conversations.replies" && r.FormValue("channel") == "CEVENTS" && r.FormValue("ts") == ts(0) {
					observed.Store(true)
					w.Header().Set("X-Oauth-Scopes", native.scopes)
					native.replyErr(w, code)
					return
				}
				original.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			imp.client.baseURL = proxy.URL
			liveTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
			native.convs[0].Msgs[0].Replies = append(native.convs[0].Msgs[0].Replies, fakeMsg{TS: liveTS, ThreadTS: ts(0), User: "USENDER", Text: "synthetic reply to a concurrently disappearing root"})
			now = now.Add(time.Hour)
			summary, runErr := imp.Import(t.Context(), opts)
			if code == "internal_error" {
				require.ErrorContains(runErr, "partial Slack sync")
				assert.Positive(summary.FetchErrors, "the transient native fetch failure must remain visible in the archive sync result")
			} else {
				require.NoError(runErr)
			}
			require.True(observed.Load(), "the actual native importer must consume the selected root's read response")
			current, lookupErr := imp.store.GetMCPResource(t.Context(), "inbox", ownRoot.ID)
			if code == "thread_not_found" {
				require.Error(lookupErr, "positive disappearance retires this exact root")
			} else {
				require.NoError(lookupErr)
				assert.Equal(ownRoot.Reference, current.Reference, "transient native failure preserves the root lifetime")
			}
			for _, resource := range []store.MCPResource{resources["CEVENTS"], otherRoot} {
				current, lookupErr := imp.store.GetMCPResource(t.Context(), "inbox", resource.ID)
				require.NoError(lookupErr)
				assert.Equal(resource.Reference, current.Reference, "neither the parent nor the same timestamp in another channel was removed")
			}
		})
	}
}
