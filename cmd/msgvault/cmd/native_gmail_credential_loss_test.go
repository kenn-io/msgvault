package cmd

import (
	"context"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func TestManualGmailSyncCredentialLossPausesNativeAuthority(t *testing.T) {
	for _, entry := range []string{"incremental", "full"} {
		t.Run(entry, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)

			cfg := testConfigValue()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			state := invocationFromContext(ctx)
			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
				Enabled: true, Principal: "owner:synthetic-gmail-loss",
				Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
			})
			require.NoError(err)
			source, err := st.GetOrCreateSource("gmail", "owner@example.net")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
				SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail",
				NativeIdentity: source.Identifier, Known: true, MessageRead: true,
			}))
			if entry == "incremental" {
				source.SyncCursor.Valid = true
				source.SyncCursor.String = "synthetic-history"
			}
			getOAuthMgr := func(context.Context, string) (*oauth.Manager, error) {
				return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
			}
			var syncErr error
			if entry == "full" {
				syncErr = runFullSync(ctx, st, getOAuthMgr, source, state)
			} else {
				syncErr = runIncrementalSync(ctx, st, getOAuthMgr, source, state)
			}
			require.Error(syncErr)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a revoked refresh token removes prior native message-read authority")
		})
	}
}
