package gmail_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

// A long-lived native client can lose its grant after construction. The real
// OAuth refresh HTTP response must retire prior Events authority before any
// Gmail archive request succeeds; matching human-readable text is insufficient.
func TestNativeGmailRefreshRevocationRetiresAuthority(t *testing.T) {
	for _, code := range []string{"invalid_grant", "server_error"} {
		t.Run(code, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "owner@example.net")
			require.NoError(err)
			original, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: original.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
			var refreshes, archiveRequests atomic.Int64
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					archiveRequests.Add(1)
					http.Error(w, "unexpected native archive request", http.StatusBadRequest)
					return
				}
				refreshes.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
			}))
			t.Cleanup(provider.Close)
			config := &oauth2.Config{ClientID: "synthetic-client", ClientSecret: "synthetic-secret", Endpoint: oauth2.Endpoint{TokenURL: provider.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
			native := gmail.NewNativeRefreshFixtureClient(provider.URL, config.TokenSource(t.Context(), &oauth2.Token{AccessToken: "expired-synthetic", RefreshToken: "synthetic-refresh", Expiry: time.Unix(1, 0)}))
			t.Cleanup(func() { assert.NoError(native.Close()) })
			opts := msgsync.DefaultOptions()
			opts.NoResume = true
			_, err = msgsync.New(native, st, opts).Full(t.Context(), source.Identifier)
			require.Error(err)
			assert.Positive(refreshes.Load(), "native OAuth refresh must actually execute")
			assert.Zero(archiveRequests.Load(), "refresh failure must prevent native archive HTTP reads")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			if code == "invalid_grant" {
				assert.Equal(int64(1), refreshes.Load(), "explicit native revocation must not be retried as a network failure")
				assert.NotEqual(original.Reference, current.Reference, "explicit OAuth revocation must retire retained authority")
				assert.False(allowed)
			} else {
				assert.Equal(original.Reference, current.Reference, "temporary OAuth server errors are not native revocation")
				assert.True(allowed)
			}
		})
	}
}
