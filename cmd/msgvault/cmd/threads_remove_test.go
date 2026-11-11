package cmd

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/threads"
)

func TestThreadsRemovalDeletesOnlyMatchingToken(t *testing.T) {
	require := require.New(t)
	directory := t.TempDir()
	cfg := &config.Config{HomeDir: directory, Data: config.DataConfig{DataDir: directory}}
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(directory, "msgvault.db"))
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, cfg.TokensDir())
	_, err := executeAddThreadsFixture(t, deps, "123")
	require.NoError(err)
	_, err = executeAddThreadsFixture(t, deps, "124")
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	require.NoError(st.Close())
	root := newTestRootCmd()
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.AddCommand(newRemoveAccountLocalTestCmd())
	root.SetArgs([]string{"remove-account", "--source-id", strconv.FormatInt(source.ID, 10), "--yes"})
	require.NoError(root.Execute())
	manager := threads.NewTokenManager(cfg.TokensDir())
	_, err = manager.Load("123", "20")
	require.ErrorIs(err, threads.ErrTokenNotFound)
	_, err = manager.Load("124", "20")
	require.NoError(err, "other app binding must remain")
	reopened, err := store.Open(filepath.Join(directory, "msgvault.db"))
	require.NoError(err)
	defer func() { require.NoError(reopened.Close()) }()
	_, err = reopened.GetSourceByID(source.ID)
	require.ErrorIs(err, store.ErrSourceNotFound)
}

func TestThreadsRemovalKeepsResolvedSourceWhenDisplayNameMoves(t *testing.T) {
	for _, removeOriginal := range []bool{false, true} {
		t.Run(strconv.FormatBool(removeOriginal), func(t *testing.T) {
			require := require.New(t)
			directory := t.TempDir()
			cfg := &config.Config{HomeDir: directory, Data: config.DataConfig{DataDir: directory}}
			path := filepath.Join(directory, "msgvault.db")
			st := testutil.NewSQLiteTestStoreAt(t, path)
			first, err := st.GetOrCreateSource(sourceTypeThreads, "threads:123:20")
			require.NoError(err)
			second, err := st.GetOrCreateSource(sourceTypeThreads, "threads:124:21")
			require.NoError(err)
			require.NoError(st.UpdateSourceDisplayName(first.ID, "Shared"))
			// Model the selector moving after resolution and before the locked continuation.
			require.NoError(st.UpdateSourceDisplayName(first.ID, "Original"))
			require.NoError(st.UpdateSourceDisplayName(second.ID, "Shared"))
			if removeOriginal {
				_, _, err = st.RemoveSourceSerialized(t.Context(), first.ID)
				require.NoError(err)
			}
			require.NoError(st.Close())
			root := newTestRootCmd()
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			cmd := newRemoveAccountLocalTestCmd()
			cmd.RunE = func(cmd *cobra.Command, args []string) error {
				return runRemoveAccountLocalWithLifecycleLock(cmd, args, true, first)
			}
			root.AddCommand(cmd)
			root.SetArgs([]string{"remove-account", "Shared", "--yes"})
			err = root.Execute()
			if removeOriginal {
				require.ErrorIs(err, store.ErrSourceNotFound)
			} else {
				require.NoError(err)
			}
			reopened, err := store.Open(path)
			require.NoError(err)
			defer func() { require.NoError(reopened.Close()) }()
			_, err = reopened.GetSourceByID(first.ID)
			require.ErrorIs(err, store.ErrSourceNotFound)
			_, err = reopened.GetSourceByID(second.ID)
			require.NoError(err)
		})
	}
}
