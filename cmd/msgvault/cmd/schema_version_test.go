package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestSchemaVersionExpectedSkipsConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cmd := newSchemaVersionCommand()
	assert.True(skipsConfigLoad(cmd))
	require.NoError(cmd.Flags().Set("database", "true"))
	assert.False(skipsConfigLoad(cmd), "database mode needs the local configuration")

	root := newRootCommand()
	root.SetErr(io.Discard)
	root.AddCommand(newSchemaVersionCommand())
	badConfig := filepath.Join(t.TempDir(), "invalid.toml")
	require.NoError(os.WriteFile(badConfig, []byte("not = [valid"), 0o600))
	root.SetArgs([]string{"--config", badConfig, "schema-version"})
	var out bytes.Buffer
	root.SetOut(&out)
	require.NoError(executeRootContext(t.Context(), root), "expected-version probe must bypass config loading")
	assert.Equal(fmt.Sprintf("%d\n", store.SchemaVersion), out.String())

	cmd = newSchemaVersionCommand()
	out.Reset()
	cmd.SetOut(&out)
	require.NoError(cmd.Execute())
	assert.Equal(fmt.Sprintf("%d\n", store.SchemaVersion), out.String())
	require.NoError(cmd.Flags().Set("database", "true"))
	assert.False(skipsConfigLoad(cmd))
}

func TestSchemaVersionDatabaseDoesNotMigrate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	conf := lifecycleTestConfig(t.TempDir())
	cmd := commandWithInvocation(t, newSchemaVersionCommand(), conf, invocationOptions{})
	cmd.SetArgs([]string{"--database"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.Error(cmd.Execute())
	_, err := os.Stat(conf.DatabaseDSN())
	assert.True(os.IsNotExist(err), "probe must not create missing archive")
	s, err := store.OpenForTest(conf.DatabaseDSN())
	require.NoError(err)
	_, err = s.DB().Exec("CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES ('keep')")
	require.NoError(err)
	require.NoError(s.Close())
	for _, version := range []int{0, store.SchemaVersion, store.SchemaVersion + 1} {
		s, err = store.OpenForTest(conf.DatabaseDSN())
		require.NoError(err)
		_, err = s.DB().Exec(fmt.Sprintf("PRAGMA user_version = %d", version))
		require.NoError(err)
		require.NoError(s.Close())
		cmd = commandWithInvocation(t, newSchemaVersionCommand(), conf, invocationOptions{})
		cmd.SetArgs([]string{"--database"})
		out.Reset()
		cmd.SetOut(&out)
		require.NoError(cmd.Execute())
		assert.Equal(fmt.Sprintf("%d\n", version), out.String())
		s, err = store.OpenReadOnly(conf.DatabaseDSN())
		require.NoError(err)
		var count int
		require.NoError(s.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE name='messages'").Scan(&count))
		assert.Zero(count)
		require.NoError(s.Close())
	}
	records, err := listLiveDaemonRuntimeRecords(conf.Data.DataDir)
	require.NoError(err)
	assert.Empty(records, "read-only probe must not start a daemon")
}

func TestSchemaVersionDatabaseCancellationBeforeOpen(t *testing.T) {
	require := require.New(t)
	conf := lifecycleTestConfig(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := newSchemaVersionCommand()
	cmd.SetContext(testInvocationContext(ctx, conf, invocationOptions{}))
	cmd.SetArgs([]string{"--database"})

	require.ErrorIs(cmd.Execute(), context.Canceled)
	_, err := os.Stat(conf.DatabaseDSN())
	require.ErrorIs(err, os.ErrNotExist, "cancellation before open must not create an archive")
}

func TestSchemaVersionRemoteRefusal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	conf := lifecycleTestConfig(t.TempDir())
	conf.Remote.URL = "http://127.0.0.1:1"
	cmd := commandWithInvocation(t, newSchemaVersionCommand(), conf, invocationOptions{})
	cmd.SetArgs([]string{"--database"})
	require.ErrorContains(cmd.Execute(), "local-only")
	s, err := store.OpenForTest(conf.DatabaseDSN())
	require.NoError(err)
	require.NoError(s.Close())
	cmd = commandWithInvocation(t, newSchemaVersionCommand(), conf, invocationOptions{useLocal: true})
	cmd.SetArgs([]string{"--database"})
	var localOut bytes.Buffer
	cmd.SetOut(&localOut)
	require.NoError(cmd.Execute())
	assert.Equal("0\n", localOut.String())
	cmd = commandWithInvocation(t, newSchemaVersionCommand(), conf, invocationOptions{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(cmd.Execute(), "binary probe is independent of remote")
	assert.Equal(fmt.Sprintf("%d\n", store.SchemaVersion), out.String())
}
