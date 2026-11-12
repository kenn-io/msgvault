package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationHubOrigin(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"https://hub.example.test/", "https://hub.example.test", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", true},
		{"http://hub.example.test", "", false}, {"https://hub.example.test/path", "", false},
		{"https://hub.example.test/?query=1", "", false}, {"https://user@hub.example.test", "", false}, {"https://hub.example.test/#", "", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := federationHubOrigin(tc.input)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestFederationEnrollOutput(t *testing.T) {
	st := testutil.NewTestStore(t)
	serverCfg := config.NewDefaultConfig()
	serverCfg.Data.DataDir = t.TempDir()
	serverCfg.Federation.Role = "hub"
	serverCfg.Server.APIKey = "owner-fixture"
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: serverCfg, Store: st, FederationStore: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, apiServer.Shutdown(context.WithoutCancel(t.Context()))) })
	srv := httptest.NewServer(apiServer.Router())
	t.Cleanup(srv.Close)
	clientCfg := config.NewDefaultConfig()
	clientCfg.Data.DataDir = t.TempDir()
	clientCfg.Remote = config.RemoteConfig{URL: srv.URL, APIKey: "owner-fixture", AllowInsecure: true}
	ctx := withStoreResolverConfig(t, clientCfg)
	template, _, err := rootCmd.Find([]string{"federation", "enroll"})
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	command.Flags().AddFlagSet(template.Flags())
	command.Flags().VisitAll(func(flag *pflag.Flag) { require.NoError(t, flag.Value.Set(flag.DefValue)); flag.Changed = false })
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice"})
	require.NoError(t, command.ExecuteContext(ctx))
	token := strings.TrimSpace(stdout.String())
	assert.True(t, strings.HasPrefix(token, "mvf1_"))
	assert.NotContains(t, stderr.String(), token)
	assert.Contains(t, stderr.String(), "join <hub-url>")
	assert.NotContains(t, stderr.String(), srv.URL)
}
