package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// An explicit OAuth invalid_grant retires native authority. Other refresh
// failures pause it without asserting account removal, and a stale refresh
// cannot retire a replacement account lifetime. The actual token parser and
// OAuth HTTP refresh run against a reserved synthetic token fixture.
func TestScheduledGmailNativeOAuthRevocation(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		stale      bool
	}{
		{name: "explicit grant revocation", code: "invalid_grant"},
		{name: "unknown provider failure", code: "server_error"},
		{name: "client configuration failure", code: "invalid_client"},
		{name: "stale grant revocation", code: "invalid_grant", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			tokenPath, restore := seedTokenEnv(t, `{"access_token":"expired-synthetic-access","refresh_token":"synthetic-refresh","expiry":"2000-01-01T00:00:00Z","scopes":["https://www.googleapis.com/auth/gmail.readonly"]}`)
			defer restore()
			originalToken, err := os.ReadFile(tokenPath)
			require.NoError(err)
			cfg := testConfigValue()
			cfg.OAuth.ClientSecrets = filepath.Join(filepath.Dir(filepath.Dir(tokenPath)), "client_secret.json")
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", scopeEscalationAccount)
			require.NoError(err)
			original, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: original.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			// Provider requests run on another goroutine: report boundary failures via
			// the real HTTP response, and inspect final Store state in the test thread.
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stale {
					if err := st.UpdateSourceIdentifier(source.ID, "replacement@example.net"); err != nil {
						http.Error(w, "synthetic replacement failed", http.StatusInternalServerError)
						return
					}
					if err := st.UpdateSourceIdentifier(source.ID, source.Identifier); err != nil {
						http.Error(w, "synthetic replacement failed", http.StatusInternalServerError)
						return
					}
					fresh, err := st.GetMCPResource(r.Context(), "source", source.ID)
					if err != nil {
						http.Error(w, "synthetic replacement unavailable", http.StatusInternalServerError)
						return
					}
					observation.SourceReference = fresh.Reference
					if err := st.ObserveNativeSourcePermission(r.Context(), observation); err != nil {
						http.Error(w, "synthetic grant failed", http.StatusInternalServerError)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.code})
			}))
			t.Cleanup(provider.Close)
			var secrets map[string]map[string]any
			require.NoError(json.Unmarshal([]byte(fakeClientSecrets), &secrets))
			secrets["installed"]["token_uri"] = provider.URL
			encoded, err := json.Marshal(secrets)
			require.NoError(err)
			require.NoError(os.WriteFile(cfg.OAuth.ClientSecrets, encoded, 0600))
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			_, err = runScheduledGmailSync(ctx, source.Identifier, source, st, oauthManagerCache(invocationFromContext(ctx)), invocationFromContext(ctx))
			require.Error(err)
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			if tc.stale {
				assert.NotEqual(original.Reference, current.Reference)
				assert.True(allowed, "stale refresh failure must not retire current native authority")
			} else {
				assert.False(allowed)
				if tc.code == "invalid_grant" {
					assert.NotEqual(original.Reference, current.Reference, "explicit revocation must retire old subscriptions and receipt lifetimes")
				} else {
					assert.Equal(original.Reference, current.Reference, "unknown refresh failure must not assert native removal")
				}
			}
			finalToken, err := os.ReadFile(tokenPath)
			require.NoError(err)
			assert.Equal(originalToken, finalToken, "failed refresh must not modify native credentials")
		})
	}
}
