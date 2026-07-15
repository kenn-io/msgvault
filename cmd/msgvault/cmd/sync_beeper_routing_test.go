package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveBeeperSyncAccountsValidatesAndDeduplicatesExplicitIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	_, err := st.GetOrCreateSource(sourceTypeBeeper, "signal")
	require.NoError(err)
	_, err = st.GetOrCreateSource(sourceTypeBeeper, "telegram")
	require.NoError(err)

	accounts, err := resolveBeeperSyncAccounts(st, []string{"signal", "signal", "telegram"})
	require.NoError(err)
	assert.Equal([]string{"signal", "telegram"}, accounts)

	_, err = resolveBeeperSyncAccounts(st, []string{"signal", "typo"})
	require.ErrorContains(err, `beeper account "typo" is not registered`)
}

func TestScheduledBeeperAttemptsRebuildAfterPartialFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var attempted []string
	rebuilds := 0
	err := runScheduledBeeperAttempts(
		context.Background(),
		[]string{"signal", "telegram"},
		func(accountID string) error {
			attempted = append(attempted, accountID)
			if accountID == "signal" {
				return errors.New("partial sync")
			}
			return nil
		},
		func() error {
			rebuilds++
			return nil
		},
	)

	require.ErrorContains(err, "beeper signal: partial sync")
	assert.Equal([]string{"signal", "telegram"}, attempted, "one failure must not starve later accounts")
	assert.Equal(1, rebuilds, "any attempted import may write messages and must trigger a cache rebuild")
}

func TestScheduledBeeperAttemptsReturnsRefreshError(t *testing.T) {
	importErr := errors.New("partial sync")
	refreshErr := errors.New("refresh failed")

	err := runScheduledBeeperAttempts(
		context.Background(),
		[]string{"signal"},
		func(string) error { return importErr },
		func() error { return refreshErr },
	)

	require.ErrorIs(t, err, importErr)
	require.ErrorIs(t, err, refreshErr)
}

func resetSyncBeeperRoutingGlobals(t *testing.T) {
	t.Helper()
	oldLimit := syncBeeperLimit
	oldFull := syncBeeperFull
	oldAccounts := syncBeeperAccounts
	t.Cleanup(func() {
		syncBeeperLimit = oldLimit
		syncBeeperFull = oldFull
		syncBeeperAccounts = oldAccounts
	})
	syncBeeperLimit = 0
	syncBeeperFull = false
	syncBeeperAccounts = nil
}

func TestSyncBeeperCommandUsesDaemonRunner(t *testing.T) {
	assert := assert.New(t)

	resetSyncBeeperRoutingGlobals(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"sync-beeper",
			"--account=signal",
			"--account=telegram",
			"--full",
			"--limit=25",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Syncing Beeper account signal\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newSyncBeeperCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{
		"--account", "signal",
		"--account", "telegram",
		"--full",
		"--limit", "25",
	})

	require.NoError(t, cmd.Execute(), "sync-beeper")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Syncing Beeper account signal")
}

func TestAddBeeperCommandForwardsTokenEnv(t *testing.T) {
	assert := assert.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{"add-beeper"}, req.Args, "args")
		assert.Equal("test-token-123", req.Env[clirun.EnvBeeperToken], "token env forwarded")
	}, `{"type":"stdout","data":"Added signal\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(clirun.EnvBeeperToken, "test-token-123")

	var stdout bytes.Buffer
	cmd := newAddBeeperCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})

	// The test harness runs in remote-daemon mode, so the local Beeper
	// Desktop preflight is skipped and validation is deferred to the daemon.
	require.NoError(t, cmd.Execute(), "add-beeper")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Added signal")
}
