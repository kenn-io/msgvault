package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestTumblrRequiredBlogAndRemoteRouting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	command := newImportTumblrCmd()
	command.SetArgs([]string{"/fixture"})
	require.ErrorContains(command.Execute(), `required flag(s) "blog" not set`)
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	command = newImportTumblrCmd()
	command.SetContext(configureRemoteDaemonForTest(t, server.URL))
	command.SetArgs([]string{"--blog", "orchard-example", "/fixture"})
	require.ErrorContains(command.Execute(), "run it on the daemon host with --local")
	assert.Zero(requests.Load())
}
func TestTumblrCLIImportIdentityAndMaintenance(t *testing.T) {
	for _, tc := range []struct {
		name                string
		noIdentity, partial bool
	}{
		{name: "identity"},
		{name: "no-default-identity", noIdentity: true},
		{name: "partial-failure", partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := lifecycleTestConfig(t.TempDir())
			cmd := &cobra.Command{Use: "import-tumblr"}
			cmd.SetContext(withStoreResolverConfig(t, cfg))
			var out bytes.Buffer
			cmd.SetOut(&out)
			fixture, err := filepath.Abs("../../../internal/tumblr/testdata/observed-shape")
			require.NoError(err)
			if tc.partial {
				root := t.TempDir()
				require.NoError(os.CopyFS(root, os.DirFS(fixture)))
				require.NoError(os.WriteFile(filepath.Join(root, "conversations", "z.html"), []byte("broken"), 0600))
				fixture = root
			}
			err = runImportTumblr(cmd, fixture, tumblrCLIOptions{Blog: "orchard-example", NoDefaultIdentity: tc.noIdentity})
			if tc.partial {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			assert.Contains(out.String(), "4 added, 0 present")
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			defer func() { require.NoError(st.Close()) }()
			var sourceID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM sources WHERE source_type = 'tumblr'").Scan(&sourceID))
			identities, err := st.ListAccountIdentitiesContext(t.Context(), sourceID)
			require.NoError(err)
			if tc.noIdentity {
				assert.Empty(identities)
			} else {
				require.Len(identities, 1)
				assert.Contains(out.String(), "Confirmed identity id:opaque-owner")
				db, err := sql.Open("duckdb", "")
				require.NoError(err)
				defer func() { require.NoError(db.Close()) }()
				var n int
				require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
				assert.Equal(4, n)
			}
		})
	}
}

func TestTumblrRootInvocation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command, _, err := rootCmd.Find([]string{"import-tumblr"})
	require.NoError(err)
	homeFlag := rootCmd.PersistentFlags().Lookup("home")
	previous, changed := homeFlag.Value.String(), homeFlag.Changed
	out, errOut := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		command.Flags().VisitAll(func(flag *pflag.Flag) { require.NoError(flag.Value.Set(flag.DefValue)); flag.Changed = false })
		require.NoError(homeFlag.Value.Set(previous))
		homeFlag.Changed = changed
		rootCmd.SetOut(out)
		rootCmd.SetErr(errOut)
		rootCmd.SetArgs(nil)
	})
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stdout)
	fixture, err := filepath.Abs("../../../internal/tumblr/testdata/observed-shape")
	require.NoError(err)
	rootCmd.SetArgs([]string{"--home", t.TempDir(), "import-tumblr", "--blog", "orchard-example", fixture})
	require.NoError(rootCmd.ExecuteContext(t.Context()))
	assert.Contains(stdout.String(), "4 added, 0 present")
}
