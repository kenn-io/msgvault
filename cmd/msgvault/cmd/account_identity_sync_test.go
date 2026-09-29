package cmd

import (
	"io"
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAddIMAPDefaultIdentityScheduledSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	savedHost, savedPort, savedUsername := imapHost, imapPort, imapUsername
	savedNoTLS, savedSTARTTLS, savedNoDefault := imapNoTLS, imapSTARTTLS, noDefaultIdentityAddImap
	t.Cleanup(func() {
		imapHost, imapPort, imapUsername = savedHost, savedPort, savedUsername
		imapNoTLS, imapSTARTTLS, noDefaultIdentityAddImap = savedNoTLS, savedSTARTTLS, savedNoDefault
	})
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("MSGVAULT_IMAP_PASSWORD", testutil.IMAPTestPassword)
	addr, _ := testutil.StartIMAPMemServerWithSpecialUse(t, map[string]int{"INBOX": 1}, nil)
	host, port, err := net.SplitHostPort(addr)
	require.NoError(err)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})

	// Re-registering saves the new flag value, including re-enabling defaults.
	for _, optOut := range []bool{true, false} {
		cmd := newAddIMAPCmd()
		cmd.SetArgs([]string{"--host", host, "--port", port, "--username", testutil.IMAPTestUsername,
			"--no-tls", "--no-default-identity=" + strconv.FormatBool(optOut)})
		require.NoError(cmd.ExecuteContext(ctx))
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
		sources, err := st.ListSources(sourceTypeIMAP)
		require.NoError(err)
		require.Len(sources, 1)
		src := sources[0]
		// Removing the last identity on an account that did not opt out must
		// still allow the next scheduled sync to restore it.
		if !optOut {
			removed, err := st.RemoveAccountIdentity(src.ID, testutil.IMAPTestUsername)
			require.NoError(err)
			require.EqualValues(1, removed)
		}
		summary, err := runScheduledIMAPSync(ctx, src, st, invocationFromContext(ctx))
		require.NoError(err)
		assert.Zero(summary.Errors)
		ids, err := st.ListAccountIdentities(src.ID)
		require.NoError(err)
		if optOut {
			assert.Empty(ids, "scheduled sync must preserve the opt-out")
		} else {
			require.Len(ids, 1)
			assert.Equal(testutil.IMAPTestUsername, ids[0].Address)
		}
		require.NoError(st.Close())
	}
}

func TestAddMicrosoftDefaultIdentityOptOut(t *testing.T) {
	savedO365, savedTeams := noDefaultIdentityAddO365, noDefaultIdentityAddTeams
	savedO365Headless, savedTeamsHeadless := o365Headless, teamsHeadless
	savedO365Tenant, savedTeamsTenant := o365TenantID, teamsTenantID
	t.Cleanup(func() {
		noDefaultIdentityAddO365, noDefaultIdentityAddTeams = savedO365, savedTeams
		o365Headless, teamsHeadless = savedO365Headless, savedTeamsHeadless
		o365TenantID, teamsTenantID = savedO365Tenant, savedTeamsTenant
	})
	for _, tc := range []struct {
		name       string
		newCommand func() *cobra.Command
	}{
		{"o365", newAddO365LocalCmd},
		{"teams", newAddTeamsLocalCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			home := t.TempDir()
			cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home},
				Microsoft: config.MicrosoftConfig{ClientID: "synthetic-client"}}
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			const email = "user@example.com"
			mgr := microsoft.NewManager(cfg.Microsoft.ClientID, "common", cfg.Microsoft.EffectiveRedirectURI(), cfg.TokensDir(), testDiscardLogger())
			require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
			require.NoError(os.WriteFile(mgr.TokenPath(email), []byte(`{"access_token":"synthetic-token"}`), 0600))
			cmd := tc.newCommand()
			cmd.SetArgs([]string{email, "--" + oauthPreflightedFlag, "--no-default-identity"})
			require.NoError(cmd.ExecuteContext(ctx))
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			t.Cleanup(func() { _ = st.Close() })
			sources, err := st.ListSources("")
			require.NoError(err)
			require.Len(sources, 1)
			confirmDefaultIdentity(io.Discard, st, sources[0].ID, email, email, "account-identifier", testDiscardLogger())
			ids, err := st.ListAccountIdentities(sources[0].ID)
			require.NoError(err)
			assert.Empty(ids, "scheduled sync must preserve the opt-out")
		})
	}
}
