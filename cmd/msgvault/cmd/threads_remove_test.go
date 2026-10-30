package cmd

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/threads"
)

func TestThreadsOAuthAdmissionAndRemoval(t *testing.T) {
	require := require.New(t)
	directory := t.TempDir()
	cfg := &config.Config{HomeDir: directory, Data: config.DataConfig{DataDir: directory}}
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(directory, "msgvault.db"))
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, cfg.TokensDir())
	_, err := executeAddThreadsFixture(t, deps, "123", false)
	require.NoError(err)
	_, err = executeAddThreadsFixture(t, deps, "124", false)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
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
