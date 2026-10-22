package microsoft

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"golang.org/x/oauth2"
)

// The token response can narrow permission during an otherwise successful
// native refresh. Today's requested scopes cannot override that response.
func TestNativeGraphRefreshUsesReturnedGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response any
		want     []string
	}{
		{"narrowed", "User.Read Mail.ReadBasic", []string{"User.Read", "Mail.ReadBasic"}},
		{"explicit empty", "", nil},
		{"omitted", nil, GraphMailScopes()},
		{"malformed", 123, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Assert.Equal(t, "/synthetic-tenant/oauth2/v2.0/token", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				response := map[string]any{"access_token": "synthetic-refreshed", "token_type": "Bearer", "expires_in": 3600}
				if tc.response != nil {
					response["scope"] = tc.response
				}
				Assert.NoError(t, json.NewEncoder(w).Encode(response))
			}))
			t.Cleanup(provider.Close)
			manager := NewGraphMailManager("synthetic-client", "common", "", t.TempDir(), nil)
			manager.authorityURL = provider.URL
			require.NoError(manager.saveToken("owner@example.net", &oauth2.Token{AccessToken: "synthetic-expired", RefreshToken: "synthetic-refresh", Expiry: time.Unix(1, 0)}, GraphMailScopes(), "synthetic-tenant"))
			access, err := manager.TokenSource(t.Context(), "owner@example.net")
			require.NoError(err)
			token, err := access(t.Context())
			require.NoError(err)
			assert.Equal("synthetic-refreshed", token)
			granted, err := manager.GrantedScopes("owner@example.net")
			require.NoError(err)
			assert.Equal(tc.want, granted)
		})
	}
}
