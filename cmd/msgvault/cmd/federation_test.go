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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationEnrollOutput(t *testing.T) {
	setup := func(t *testing.T) (*store.Store, *cobra.Command, context.Context) {
		t.Helper()
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
		command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
		silenceUsageInRunE(command)
		command.Flags().AddFlagSet(template.Flags())
		command.Flags().VisitAll(func(flag *pflag.Flag) { require.NoError(t, flag.Value.Set(flag.DefValue)); flag.Changed = false })
		t.Cleanup(func() {
			command.Flags().VisitAll(func(flag *pflag.Flag) { assert.NoError(t, flag.Value.Set(flag.DefValue)); flag.Changed = false })
		})
		return st, command, ctx
	}
	t.Run("token and join hint", func(t *testing.T) {
		for _, tc := range []struct {
			name, spokeName string
		}{{"matching", ""}, {"distinct", "laptop"}} {
			t.Run(tc.name, func(t *testing.T) {
				assertions := assert.New(t)
				_, command, ctx := setup(t)
				stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
				command.SetOut(stdout)
				command.SetErr(stderr)
				command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice", "--spoke-name", tc.spokeName})
				require.NoError(t, command.ExecuteContext(ctx))
				token := strings.TrimSpace(stdout.String())
				assertions.Regexp(`^mvf1_[A-Za-z0-9_-]+$`, token)
				assertions.Equal(token+"\n", stdout.String())
				assertions.NotContains(stderr.String(), token)
				assertions.Contains(stderr.String(), "Member: alice\n")
				if tc.spokeName == "" {
					assertions.NotContains(stderr.String(), "Spoke:")
				} else {
					assertions.Contains(stderr.String(), "Spoke: laptop\n")
				}
				assertions.Contains(stderr.String(), "msgvault federation join <hub-url>")
				assertions.NotContains(stderr.String(), "http://")
			})
		}
	})
	t.Run("list", func(t *testing.T) {
		requirements := require.New(t)
		st, _, ctx := setup(t)
		matching, _, err := st.CreateFederationEnrollmentContext(t.Context(), strings.Repeat("a", 64), "alice", "")
		requirements.NoError(err)
		secondMatching, _, err := st.CreateFederationEnrollmentContext(t.Context(), strings.Repeat("c", 64), "alice", "")
		requirements.NoError(err)
		distinct, _, err := st.CreateFederationEnrollmentContext(t.Context(), strings.Repeat("b", 64), "bob", "laptop")
		requirements.NoError(err)
		template, _, err := rootCmd.Find([]string{"federation", "enrollments", "list"})
		requirements.NoError(err)
		list := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
		list.Flags().AddFlagSet(template.Flags())
		var output bytes.Buffer
		list.SetOut(&output)
		requirements.NoError(list.ExecuteContext(ctx))
		assert.Contains(t, output.String(), matching.EnrollmentUID+"\t"+strings.Repeat("a", 64)+"\talice\tactive\n")
		assert.Contains(t, output.String(), secondMatching.EnrollmentUID+"\t"+strings.Repeat("c", 64)+"\talice\tactive\n")
		assert.Contains(t, output.String(), distinct.EnrollmentUID+"\t"+strings.Repeat("b", 64)+"\tbob (laptop)\tactive\n")
	})
	t.Run("server error", func(t *testing.T) {
		_, command, ctx := setup(t)
		command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", " "})
		assert.ErrorContains(t, command.ExecuteContext(ctx), "member")
	})
}
