package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func seedManualGmailAuthority(t *testing.T, st *store.Store, email string) (*store.Source, string) {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceTypeGmail, email)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncCursor(source.ID, "12345"))
	source, err = st.GetSourceByID(source.ID)
	require.NoError(t, err)
	resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(t, err)
	require.NoError(t, st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceTypeGmail, NativeIdentity: email, Known: true, MessageRead: true}))
	return source, resource.Reference
}

func manualGmailCredentialConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfigValue()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = false
	cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "client_secret.json")
	require.NoError(t, os.WriteFile(cfg.OAuth.ClientSecrets, []byte(fakeClientSecrets), 0o600))
	return cfg
}

// Run the production manual sync runners and real credential loaders. A factory
// error stands in only for an external provider timeout or concurrent source
// replacement, keeping permission persistence and lifetime fencing unchanged.
func TestManualGmailCredentialLossPausesNativeAuthority(t *testing.T) {
	for _, entry := range []string{"full", "incremental"} {
		for _, mode := range []string{"missing_token", "malformed_token", "missing_configuration", "unconfigured", "missing_service_account", "missing_invocation", "transient_network", "replaced_source"} {
			t.Run(entry+"/"+mode, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				cfg := manualGmailCredentialConfig(t)
				ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
				state := invocationFromContext(ctx)
				st := testutil.NewTestStore(t)
				source, reference := seedManualGmailAuthority(t, st, "manual@example.net")
				other, _ := seedManualGmailAuthority(t, st, "other@example.net")
				getManager := oauthManagerCache(state)
				var cause error
				switch mode {
				case "malformed_token":
					require.NoError(os.MkdirAll(cfg.TokensDir(), 0o700))
					require.NoError(os.WriteFile(filepath.Join(cfg.TokensDir(), source.Identifier+".json"), []byte(`{"access_token":`), 0o600))
				case "missing_configuration":
					cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "missing-client.json")
				case "unconfigured":
					cfg.OAuth = config.OAuthConfig{}
				case "missing_service_account":
					cfg.OAuth.ServiceAccountKey = filepath.Join(cfg.HomeDir, "missing-service-account.json")
				case "missing_invocation":
					state.cfg = nil
				case "transient_network":
					cause = &net.DNSError{IsTimeout: true, Err: "synthetic transient timeout"}
					getManager = func(context.Context, string) (*oauth.Manager, error) { return nil, cause }
				case "replaced_source":
					cause = errors.New("synthetic stale credential failure")
					getManager = func(context.Context, string) (*oauth.Manager, error) {
						require.NoError(st.UpdateSourceIdentifier(source.ID, "replacement@example.net"))
						require.NoError(st.UpdateSourceIdentifier(source.ID, source.Identifier))
						_, _ = seedManualGmailAuthority(t, st, source.Identifier)
						return nil, cause
					}
				}
				var err error
				if entry == "full" {
					err = runFullSync(ctx, st, getManager, source, state)
				} else {
					err = runIncrementalSync(ctx, st, getManager, source, state)
				}
				require.Error(err)
				if cause != nil {
					require.ErrorIs(err, cause)
				}
				if mode == "malformed_token" {
					require.ErrorIs(err, oauth.ErrInvalidTokenJSON, "the real token parser must reject the fixture")
				}
				allowed, permissionErr := st.NativeSourceMessagePermission(t.Context(), source.ID)
				require.NoError(permissionErr)
				assert.Equal(mode == "transient_network" || mode == "replaced_source", allowed, "manual credential failures must pause only current local credential loss")
				current, resourceErr := st.GetMCPResource(t.Context(), "source", source.ID)
				require.NoError(resourceErr)
				if mode == "replaced_source" {
					assert.NotEqual(reference, current.Reference)
				} else {
					assert.Equal(reference, current.Reference, "local credential failure does not prove account removal")
				}
				otherAllowed, permissionErr := st.NativeSourceMessagePermission(t.Context(), other.ID)
				require.NoError(permissionErr)
				assert.True(otherAllowed, "another account retains independent authority")
			})
		}
	}
}

// Exercise command source discovery as well as the runner: neither a full-sync
// preflight nor all-account discovery may bypass the pinned credential failure.
func TestManualGmailCommandsPauseCredentialFailures(t *testing.T) {
	for _, entry := range []string{"full", "incremental"} {
		for _, selection := range []string{"explicit", "all"} {
			for _, mode := range []string{"missing_configuration", "unconfigured", "missing_token"} {
				t.Run(entry+"/"+selection+"/"+mode, func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					cfg := manualGmailCredentialConfig(t)
					st, err := store.Open(cfg.DatabaseDSN())
					require.NoError(err)
					t.Cleanup(func() { assert.NoError(st.Close()) })
					require.NoError(st.InitSchema())
					source, reference := seedManualGmailAuthority(t, st, "manual@example.net")
					switch mode {
					case "missing_configuration":
						cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "missing-client.json")
					case "unconfigured":
						cfg.OAuth = config.OAuthConfig{}
					}
					command := &cobra.Command{Use: "sync"}
					command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
					var args []string
					if selection == "explicit" {
						args = []string{source.Identifier}
					}
					if entry == "full" {
						err = runSyncFullLocal(command, args)
					} else {
						err = runSyncIncrementalLocal(command, args)
					}
					require.Error(err)
					allowed, permissionErr := st.NativeSourceMessagePermission(t.Context(), source.ID)
					require.NoError(permissionErr)
					assert.False(allowed, "the manual command must apply local credential loss before returning")
					current, resourceErr := st.GetMCPResource(t.Context(), "source", source.ID)
					require.NoError(resourceErr)
					assert.Equal(reference, current.Reference)
				})
			}
		}
	}
}
