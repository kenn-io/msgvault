package microsoft

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"golang.org/x/oauth2"
)

// A successful native refresh is not proof that a legacy token had the scopes
// requested by today's configuration. Preserve unknown grant metadata while
// keeping the ordinary archive credential usable. Known grants stay known.
func TestNativeGraphRefreshPreservesGrantEvidence(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy unknown", true: "native grant"}[known], func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			var calls atomic.Int64
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/token") {
					http.NotFound(w, r)
					return
				}
				calls.Add(1)
				if err := r.ParseForm(); err != nil {
					http.Error(w, "invalid synthetic request", http.StatusBadRequest)
					return
				}
				assert.Equal("refresh_token", r.Form.Get("grant_type"))
				assert.Equal("synthetic-refresh", r.Form.Get("refresh_token"))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-renewed", "token_type": "Bearer", "expires_in": 3600})
			}))
			t.Cleanup(provider.Close)
			manager := NewGraphMailManager("synthetic-client", "common", "", t.TempDir(), nil)
			manager.authorityURL = provider.URL
			var scopes []string
			if known {
				scopes = GraphMailScopes()
			}
			require.NoError(manager.saveToken("owner@example.net", &oauth2.Token{AccessToken: "synthetic-expired", RefreshToken: "synthetic-refresh", TokenType: "Bearer", Expiry: time.Unix(1, 0)}, scopes, "synthetic-tenant"))
			access, err := manager.TokenSource(t.Context(), "owner@example.net")
			require.NoError(err)
			token, err := access(t.Context())
			require.NoError(err)
			assert.Equal("synthetic-renewed", token)
			assert.Positive(calls.Load(), "native refresh HTTP must actually execute")
			saved, err := manager.loadTokenFile("owner@example.net")
			require.NoError(err)
			assert.Equal(scopes, saved.Scopes, "requested defaults must not become evidence of a native grant")
			allowed, err := manager.HasScopes("owner@example.net")
			require.NoError(err)
			assert.Equal(known, allowed)
		})
	}
}
