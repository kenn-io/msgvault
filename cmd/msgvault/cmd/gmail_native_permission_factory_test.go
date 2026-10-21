package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

// Exercise the real native token-file reader and production factories. A
// factory that drops the saved grant must make the read-positive cases fail.
func TestGmailClientFactoriesPreserveNativeSavedGrant(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scopes      []string
		read, known bool
	}{
		{"read_only", []string{oauth.ScopeGmailReadonly}, true, true},
		{"modify", []string{oauth.ScopeGmailModify}, true, true},
		{"labels_only", []string{oauth.ScopeGmailLabels}, false, true},
		{"legacy_unknown", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			token := map[string]any{"access_token": "synthetic-access", "expiry": "2099-01-01T00:00:00Z"}
			if tc.scopes != nil {
				token["scopes"] = tc.scopes
			}
			data, err := json.Marshal(token)
			require.NoError(err)
			tokenPath, restore := seedTokenEnv(t, string(data))
			defer restore()
			cfg := testConfigValue()
			cfg.OAuth.ClientSecrets = filepath.Join(filepath.Dir(filepath.Dir(tokenPath)), "client_secret.json")
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			getManager := oauthManagerCache(invocationFromContext(ctx))
			source := &store.Source{SourceType: "gmail", Identifier: scopeEscalationAccount}
			before, err := os.ReadFile(tokenPath)
			require.NoError(err)
			for _, factory := range []struct {
				name  string
				build func() (gmail.API, error)
			}{
				{"on_demand", func() (gmail.API, error) { return buildAPIClient(ctx, source, getManager, nil) }},
				{"scheduled", func() (gmail.API, error) {
					client, _, err := newDaemonGmailClient(ctx, source.Identifier, source, getManager, invocationFromContext(ctx), oauth.Scopes)
					return client, err
				}},
			} {
				t.Run(factory.name, func(t *testing.T) {
					assert, require := Assert.New(t), Require.New(t)
					client, err := factory.build()
					require.NoError(err)
					t.Cleanup(func() { require.NoError(client.Close()) })
					native, ok := client.(interface{ NativeMessageReadPermission() (bool, bool) })
					require.True(ok)
					read, known := native.NativeMessageReadPermission()
					assert.Equal(tc.read, read)
					assert.Equal(tc.known, known)
					if tc.read {
						// The token manager can observe narrower scopes during a
						// refresh. A running client must use that current metadata.
						narrowed := map[string]any{"access_token": "synthetic-access", "expiry": "2099-01-01T00:00:00Z", "scopes": []string{oauth.ScopeGmailLabels}}
						encoded, err := json.Marshal(narrowed)
						require.NoError(err)
						require.NoError(os.WriteFile(tokenPath, encoded, 0600))
						read, known = native.NativeMessageReadPermission()
						assert.False(read, "a refreshed narrower grant cannot retain message read")
						assert.True(known)
						require.NoError(os.WriteFile(tokenPath, before, 0600))
					}
				})
			}
			after, err := os.ReadFile(tokenPath)
			require.NoError(err)
			assert.Equal(before, after, "permission discovery must neither upgrade nor rewrite the native grant")
		})
	}
}
