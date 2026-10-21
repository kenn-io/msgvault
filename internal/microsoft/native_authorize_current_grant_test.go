package microsoft

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"testing"
)

// Use the native authorization persistence path and its existing browser-flow
// seam; no live authorization, grants or installed credentials are changed.
func TestNativeGraphAuthorizationUsesReturnedGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response any
		want     []string
	}{{"narrowed", "User.Read Mail.ReadBasic", []string{"User.Read", "Mail.ReadBasic"}}, {"explicit_empty", "", nil}, {"malformed", 123, nil}, {"omitted", nil, GraphMailScopes()}} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewGraphMailManager("synthetic-client", "common", "", t.TempDir(), nil)
			manager.verifyIDTokenFn = testVerifyFn
			manager.browserFlowFn = func(_ context.Context, email string, _ []string) (*oauth2.Token, string, error) {
				extra := map[string]any{"id_token": makeIDToken(t, map[string]any{"email": email, "tid": "synthetic-tenant"})}
				if tc.response != nil {
					extra["scope"] = tc.response
				}
				return (&oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", TokenType: "Bearer"}).WithExtra(extra), "test-nonce", nil
			}
			require.NoError(t, manager.Authorize(t.Context(), "owner@example.net"))
			granted, err := manager.GrantedScopes("owner@example.net")
			require.NoError(t, err)
			assert.Equal(t, tc.want, granted, "an explicit token response must replace requested permission")
		})
	}
}
