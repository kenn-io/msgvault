package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func commandWithInvocation(t *testing.T, cmd *cobra.Command, conf *config.Config, options invocationOptions) *cobra.Command {
	t.Helper()
	cmd.SetContext(testInvocationContext(t.Context(), conf, options))
	return cmd
}

func TestMigrateFreshAndRepeated(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	conf := lifecycleTestConfig(t.TempDir())
	for range 2 {
		var out bytes.Buffer
		cmd := commandWithInvocation(t, newMigrateCommand(), conf, invocationOptions{})
		cmd.SetOut(&out)
		require.NoError(cmd.Execute())
		assert.Equal(fmt.Sprintf("Schema version: %d\n", store.SchemaVersion), out.String())
	}
	s, err := store.OpenReadOnly(conf.DatabaseDSN())
	require.NoError(err)
	var version int
	require.NoError(s.DB().QueryRow("SELECT value FROM archive_metadata WHERE key='schema_version'").Scan(&version))
	assert.Equal(store.SchemaVersion, version)
	require.NoError(s.Close())
}

func TestMigrateRefusesOwnersBeforeCreatingArchive(t *testing.T) {
	for _, kind := range []string{"daemon", "writer"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			conf := lifecycleTestConfig(t.TempDir())
			if kind == "daemon" {
				owner, err := tryAcquireDaemonOwnerLock(conf.Data.DataDir)
				require.NoError(err)
				t.Cleanup(func() { require.NoError(owner.Close()) })
			} else {
				owner, err := tryAcquireWriteOwnerLock(conf.Data.DataDir)
				require.NoError(err)
				t.Cleanup(func() { require.NoError(owner.Close()) })
			}
			cmd := commandWithInvocation(t, newMigrateCommand(), conf, invocationOptions{})
			require.ErrorContains(cmd.Execute(), "daemon stop")
			_, err := os.Stat(conf.DatabaseDSN())
			assert.True(os.IsNotExist(err))
		})
	}
}

func TestMigrateDoesNotApplyLegacyIdentityConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	conf := lifecycleTestConfig(t.TempDir())
	s, err := store.OpenForTest(conf.DatabaseDSN())
	require.NoError(err)
	require.NoError(s.InitSchema())
	_, err = s.GetOrCreateSource("gmail", "account@example.com")
	require.NoError(err)
	require.NoError(s.Close())
	conf.Identity.Addresses = []string{"legacy@example.com"}
	cmd := commandWithInvocation(t, newMigrateCommand(), conf, invocationOptions{})
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(cmd.Execute())
	s, err = store.OpenReadOnly(conf.DatabaseDSN())
	require.NoError(err)
	var count int
	require.NoError(s.DB().QueryRow("SELECT count(*) FROM account_identities").Scan(&count))
	assert.Zero(count, "identity config must await source confirmation")
	require.NoError(s.Close())
}

func TestMigrateFailureReleasesLocks(t *testing.T) {
	for _, kind := range []string{"future", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			conf := lifecycleTestConfig(t.TempDir())
			ctx := t.Context()
			if kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				s, err := store.OpenForTest(conf.DatabaseDSN())
				require.NoError(err)
				_, err = s.DB().Exec("CREATE TABLE archive_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
				require.NoError(err)
				_, err = s.DB().Exec("INSERT INTO archive_metadata (key, value) VALUES ('schema_version', ?)", store.SchemaVersion+1)
				require.NoError(err)
				require.NoError(s.Close())
			}
			cmd := newMigrateCommand()
			cmd.SetContext(testInvocationContext(ctx, conf, invocationOptions{}))
			var out bytes.Buffer
			// The production root suppresses usage on runtime errors.
			cmd.SilenceUsage = true
			cmd.SetOut(&out)
			if kind == "cancelled" {
				require.ErrorIs(cmd.Execute(), context.Canceled)
				_, err := os.Stat(conf.DatabaseDSN())
				require.ErrorIs(err, os.ErrNotExist, "cancellation before open must not create an empty archive")
			} else {
				require.ErrorContains(cmd.Execute(), "newer")
			}
			assert.Empty(out.String())
			daemon, err := tryAcquireDaemonOwnerLock(conf.Data.DataDir)
			require.NoError(err)
			require.NoError(daemon.Close())
			writer, err := tryAcquireWriteOwnerLock(conf.Data.DataDir)
			require.NoError(err)
			require.NoError(writer.Close())
		})
	}
}
