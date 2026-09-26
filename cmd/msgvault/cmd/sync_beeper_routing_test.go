package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/store"
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

func TestFilterBeeperReanchorMarkedAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	blocked, err := st.GetOrCreateSource(sourceTypeBeeper, "blocked")
	require.NoError(err)
	_, err = st.GetOrCreateSource(sourceTypeBeeper, "ready")
	require.NoError(err)
	require.NoError(st.SetArchiveMarker(t.Context(), store.BeeperReanchorMarkerKey(blocked.ID),
		"manual verification required"))

	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	eligible, err := filterBeeperReanchorMarkedAccounts(t.Context(), st, []string{"blocked", "ready"})
	require.NoError(err)
	assert.Equal([]string{"ready"}, eligible)
	assert.Contains(logs.String(), "skipping scheduled Beeper sync until manual anchor verification")
	assert.Contains(logs.String(), "account=blocked")
}

func TestScheduledBeeperAttemptsRebuildAfterPartialFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var attempted []string
	rebuilds := 0
	err := runScheduledBeeperAttempts(
		context.Background(),
		[]string{"signal", "telegram"},
		&beeperAccountRotation{},
		nil,
		func(accountID string) (bool, error) {
			attempted = append(attempted, accountID)
			if accountID == "signal" {
				return false, errors.New("partial sync")
			}
			return false, nil
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
		&beeperAccountRotation{},
		nil,
		func(string) (bool, error) { return false, importErr },
		func() error { return refreshErr },
	)

	require.ErrorIs(t, err, importErr)
	require.ErrorIs(t, err, refreshErr)
}

func TestScheduledBeeperAttemptsRotatesAfterStop(t *testing.T) {
	assert := assert.New(t)
	rotation := &beeperAccountRotation{}
	accounts := []string{"a", "b", "c"}
	noRebuild := func() error { return nil }

	var first []string
	require.NoError(t, runScheduledBeeperAttempts(context.Background(), accounts, rotation, nil,
		func(id string) (bool, error) {
			first = append(first, id)
			return id == "b", nil
		}, noRebuild))
	assert.Equal([]string{"a", "b"}, first, "a stopped account ends the job")

	var second []string
	require.NoError(t, runScheduledBeeperAttempts(context.Background(), accounts, rotation, nil,
		func(id string) (bool, error) {
			second = append(second, id)
			return false, nil
		}, noRebuild))
	assert.Equal([]string{"c", "a", "b"}, second, "the next run reaches accounts after the stopped account")

	var third []string
	require.NoError(t, runScheduledBeeperAttempts(context.Background(), accounts, rotation, nil,
		func(id string) (bool, error) {
			third = append(third, id)
			return false, nil
		}, noRebuild))
	assert.Equal([]string{"a", "b", "c"}, third, "a complete run restarts from the first account")
}

func TestScheduledBeeperAttemptsStopBetweenAccounts(t *testing.T) {
	assert := assert.New(t)
	rotation := &beeperAccountRotation{}
	var attempted []string
	stopped := false
	require.NoError(t, runScheduledBeeperAttempts(context.Background(), []string{"a", "b", "c"}, rotation,
		func() bool { return stopped },
		func(id string) (bool, error) {
			attempted = append(attempted, id)
			stopped = true // budget spent by the first account
			return false, nil
		}, func() error { return nil }))
	assert.Equal([]string{"a"}, attempted)
	assert.Equal([]string{"b", "c", "a"}, rotation.order([]string{"a", "b", "c"}),
		"the next run starts with the first account not reached")
}

func TestScheduledBeeperAttemptsResumeAtAccountInterruptedByYield(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rotation := &beeperAccountRotation{}
	ctx, cancel := context.WithCancel(context.Background())
	var attempted []string
	err := runScheduledBeeperAttempts(ctx, []string{"a", "b", "c"}, rotation, nil,
		func(id string) (bool, error) {
			attempted = append(attempted, id)
			if id == "b" {
				cancel() // an API request made the scheduled job yield mid-account
				return false, context.Canceled
			}
			return false, nil
		}, func() error { return nil })
	require.ErrorIs(err, context.Canceled)
	assert.Equal([]string{"a", "b"}, attempted)
	assert.Equal([]string{"c", "a", "b"}, rotation.order([]string{"a", "b", "c"}),
		"the next run reaches accounts after the interrupted account")
}

func TestPrintBeeperSummaryReportsIdentityReplayPending(t *testing.T) {
	assert := assert.New(t)
	cmd := newSyncBeeperCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	printBeeperSummary(cmd, "telegram", &beeper.ImportSummary{
		IdentityReplayErrors: 1,
		Errors:               1,
	})

	assert.Contains(stdout.String(), "1 identity replay pending")
	assert.Contains(stdout.String(), "1 errors")
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

func TestScheduledBeeperAttemptsRotateWhenEveryAccountStops(t *testing.T) {
	rotation := &beeperAccountRotation{}
	var attempted []string
	for range 3 {
		require.NoError(t, runScheduledBeeperAttempts(t.Context(), []string{"a", "b", "c"}, rotation, nil,
			func(id string) (bool, error) {
				attempted = append(attempted, id)
				return true, nil
			}, func() error { return nil }))
	}
	assert.Equal(t, []string{"a", "b", "c"}, attempted, "each stopped account yields the next tick")
}
