package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScheduledGmailCredentialLossPausesNativeAuthority(t *testing.T) {
	for _, mode := range []string{"missing_token", "missing_configuration", "transient_network", "replaced_source"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			tokenPath, restore := seedTokenEnv(t, `{"access_token":"synthetic-access","expiry":"2099-01-01T00:00:00Z","scopes":["https://www.googleapis.com/auth/gmail.readonly"]}`)
			defer restore()
			cfg := testConfigValue()
			cfg.OAuth.ClientSecrets = filepath.Join(filepath.Dir(filepath.Dir(tokenPath)), "client_secret.json")
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			manager, err := oauth.NewManagerWithCredentials(ctx, config.OAuthApp{ClientSecrets: cfg.OAuth.ClientSecrets}, cfg.TokensDir(), cfg.OAuth.Tokens, testLoggerValue(), oauth.Scopes)
			require.NoError(err)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", scopeEscalationAccount)
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			other, err := st.GetOrCreateSource("gmail", "other@example.net")
			require.NoError(err)
			otherResource, err := st.GetMCPResource(t.Context(), "source", other.ID)
			require.NoError(err)
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: other.ID, SourceReference: otherResource.Reference, SourceType: "gmail", NativeIdentity: other.Identifier, Known: true, MessageRead: true}))
			require.NoError(os.Remove(tokenPath))
			replacedReference := ""
			_, err = runScheduledGmailSync(ctx, source.Identifier, source, st, func(context.Context, string) (*oauth.Manager, error) {
				switch mode {
				case "missing_configuration":
					return nil, errors.New("synthetic unavailable client configuration")
				case "transient_network":
					return nil, &net.DNSError{IsTimeout: true, Err: "synthetic transient timeout"}
				case "replaced_source":
					require.NoError(st.UpdateSourceIdentifier(source.ID, "replacement@example.net"))
					require.NoError(st.UpdateSourceIdentifier(source.ID, source.Identifier))
					fresh, freshErr := st.GetMCPResource(t.Context(), "source", source.ID)
					require.NoError(freshErr)
					observation.SourceReference = fresh.Reference
					replacedReference = fresh.Reference
					require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
					return nil, errors.New("synthetic stale configuration failure")
				}
				return manager, nil
			}, invocationFromContext(ctx))
			require.Error(err)
			allowed, permissionErr := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(permissionErr)
			if mode == "transient_network" || mode == "replaced_source" {
				assert.True(allowed, "unrelated failures and stale observers must not remove current native authority")
			} else {
				assert.False(allowed, "credential construction fails before Syncer, so it must pause prior Events authority")
			}
			current, resourceErr := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(resourceErr)
			if mode == "replaced_source" {
				assert.Equal(replacedReference, current.Reference)
				assert.NotEqual(resource.Reference, current.Reference)
			} else {
				assert.Equal(resource.Reference, current.Reference, "local credential loss does not prove native account removal")
			}
			otherAllowed, permissionErr := st.NativeSourceMessagePermission(t.Context(), other.ID)
			require.NoError(permissionErr)
			assert.True(otherAllowed, "another native account must retain its own authority")
			_, statErr := os.Stat(tokenPath)
			assert.ErrorIs(statErr, os.ErrNotExist, "scheduled failure must not create or replace native credentials")
		})
	}
}
