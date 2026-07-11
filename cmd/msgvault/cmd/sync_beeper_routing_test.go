package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
)

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
