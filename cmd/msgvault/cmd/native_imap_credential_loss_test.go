package cmd

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"testing"
)

// Use the real daemon and CLI credential construction paths. The failure occurs
// before any provider connection; prior-process permission must not survive it.
func TestNativeIMAPCredentialLossPausesAuthority(t *testing.T) {
	for _, entry := range []string{"scheduled", "full"} {
		for _, mode := range []string{"missing_password", "missing_config", "missing_invocation"} {
			t.Run(entry+"/"+mode, func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				cfg := testConfigValue()
				cfg.HomeDir = t.TempDir()
				cfg.Data.DataDir = cfg.HomeDir
				ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
				state := invocationFromContext(ctx)
				st := testutil.NewTestStore(t)
				imapCfg := &imapclient.Config{Host: "imap.example.net", Port: 993, Username: "receiver@example.net", TLS: true}
				source, err := st.GetOrCreateSource("imap", imapCfg.Identifier())
				require.NoError(err)
				raw, err := imapCfg.ToJSON()
				require.NoError(err)
				require.NoError(st.UpdateSourceSyncConfig(source.ID, raw))
				source, err = st.GetSourceByID(source.ID)
				require.NoError(err)
				resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
				require.NoError(err)
				require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "imap", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
				switch mode {
				case "missing_config":
					source.SyncConfig.Valid = false
				case "missing_invocation":
					state.cfg = nil
				}
				if entry == "scheduled" {
					_, err = runScheduledIMAPSync(ctx, source, st, state)
				} else {
					err = runFullSync(ctx, st, nil, source, state)
				}
				require.Error(err)
				allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
				require.NoError(err)
				assert.False(allowed, "credential failure before Syncer must pause prior Events authority")
				current, err := st.GetMCPResource(t.Context(), "source", source.ID)
				require.NoError(err)
				assert.Equal(resource.Reference, current.Reference, "missing local evidence does not prove native removal")
				_, err = os.Stat(imapclient.CredentialsPath(cfg.TokensDir(), source.Identifier))
				assert.ErrorIs(err, os.ErrNotExist)
			})
		}
	}
}
