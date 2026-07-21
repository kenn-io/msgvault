package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSplitSlackIdentifier(t *testing.T) {
	tests := []struct {
		in         string
		team, user string
		ok         bool
	}{
		{"T01:UME", "T01", "UME", true},
		{"T01:", "T01", "", false},
		{":UME", "", "UME", false},
		{"T01", "T01", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			team, user, ok := splitSlackIdentifier(tt.in)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.team, team)
				assert.Equal(t, tt.user, user)
			}
		})
	}
}

func TestResolveSlackSyncSourcesFiltersByTeam(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	_, err := resolveSlackSyncSources(st, "")
	require.ErrorContains(err, "no Slack workspaces registered")

	_, err = st.GetOrCreateSource(sourceTypeSlack, "T01:UME")
	require.NoError(err)
	_, err = st.GetOrCreateSource(sourceTypeSlack, "T02:UOTHER")
	require.NoError(err)

	all, err := resolveSlackSyncSources(st, "")
	require.NoError(err)
	assert.Len(all, 2)

	one, err := resolveSlackSyncSources(st, "T02")
	require.NoError(err)
	require.Len(one, 1)
	assert.Equal("T02:UOTHER", one[0].Identifier)

	_, err = resolveSlackSyncSources(st, "TYPO")
	require.ErrorContains(err, `slack workspace "TYPO" is not registered`)
}

func TestRunConfiguredSlackSyncIsolatesBrokenWorkspaces(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	// One malformed identifier and one workspace whose token file is
	// missing: the scheduler entrypoint must report both without panicking
	// or aborting on the first.
	_, err := st.GetOrCreateSource(sourceTypeSlack, "malformed-no-colon")
	require.NoError(err)
	_, err = st.GetOrCreateSource(sourceTypeSlack, "T09:UME")
	require.NoError(err)

	tmpDir := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	err = runConfiguredSlackSync(context.Background(), st)
	require.ErrorContains(err, "malformed identifier")
	require.ErrorContains(err, "no Slack token for UME in workspace T09")
}

func TestSlackImportOptionsDeriveFromConfig(t *testing.T) {
	assert := assert.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	media := false
	cfg = &config.Config{
		HomeDir: t.TempDir(),
		Slack: config.SlackConfig{
			Channels:        []string{"eng"},
			ExcludeChannels: []string{"noise"},
			Media:           &media,
			MaxMediaMB:      7,
		},
	}

	opts := slackImportOptions("T01", "UME")
	assert.Equal("T01", opts.TeamID)
	assert.Equal("UME", opts.UserID)
	assert.True(opts.NoMedia)
	assert.Equal(int64(7)<<20, opts.MaxMediaBytes)
	assert.Equal([]string{"eng"}, opts.IncludeChannels)
	assert.Equal([]string{"noise"}, opts.ExcludeChannels)
}

func resetSyncSlackRoutingGlobals(t *testing.T) {
	t.Helper()
	oldLimit := syncSlackLimit
	oldFull := syncSlackFull
	oldNoThreads := syncSlackNoThreads
	oldNoMedia := syncSlackNoMedia
	t.Cleanup(func() {
		syncSlackLimit = oldLimit
		syncSlackFull = oldFull
		syncSlackNoThreads = oldNoThreads
		syncSlackNoMedia = oldNoMedia
	})
	syncSlackLimit = 0
	syncSlackFull = false
	syncSlackNoThreads = false
	syncSlackNoMedia = false
}

func TestSyncSlackCommandUsesDaemonRunner(t *testing.T) {
	assert := assert.New(t)

	resetSyncSlackRoutingGlobals(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"sync-slack",
			"--full",
			"--limit=25",
			"--no-threads",
			"T0123456789",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Syncing Slack workspace T0123456789\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newSyncSlackCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{
		"T0123456789",
		"--full",
		"--limit", "25",
		"--no-threads",
	})

	require.NoError(t, cmd.Execute(), "sync-slack")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Syncing Slack workspace T0123456789")
}

func TestAddSlackCommandForwardsTokenEnv(t *testing.T) {
	assert := assert.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{"add-slack"}, req.Args, "args")
		assert.Equal("xoxp-test-123", req.Env[clirun.EnvSlackToken], "token env forwarded")
	}, `{"type":"stdout","data":"Added Slack workspace Testers\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(clirun.EnvSlackToken, "xoxp-test-123")

	var stdout bytes.Buffer
	cmd := newAddSlackCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})

	require.NoError(t, cmd.Execute(), "add-slack")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Added Slack workspace Testers")
}
