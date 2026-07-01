package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogsCommandUsesDaemonRunner(t *testing.T) {
	resetLogsRoutingGlobals(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal(t, []string{
			"logs",
			"--all",
			"--grep=sync",
			"--level=warn",
			"--lines=25",
			"--run-id=abc123",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"12:00:00 WARN abc123 sync failed\n"}`, `{"type":"stderr","data":"tail warning\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := newLogsRoutingTestCommand()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"--all",
		"--grep", "sync",
		"--level", "warn",
		"--lines", "25",
		"--run-id", "abc123",
	})

	require.NoError(t, cmd.Execute(), "logs")
	assert.Equal(t, 1, int(requests.Load()), "runner endpoint calls")
	assert.Equal(t, "12:00:00 WARN abc123 sync failed\n", stdout.String(), "stdout")
	assert.Equal(t, "tail warning\n", stderr.String(), "stderr")
}

func resetLogsRoutingGlobals(t *testing.T) {
	t.Helper()
	oldFollow := logsFollow
	oldLines := logsLines
	oldRunID := logsRunID
	oldLevel := logsLevel
	oldAll := logsAll
	oldGrep := logsGrep
	oldPath := logsPath
	t.Cleanup(func() {
		logsFollow = oldFollow
		logsLines = oldLines
		logsRunID = oldRunID
		logsLevel = oldLevel
		logsAll = oldAll
		logsGrep = oldGrep
		logsPath = oldPath
	})
	logsFollow = false
	logsLines = 50
	logsRunID = ""
	logsLevel = ""
	logsAll = false
	logsGrep = ""
	logsPath = false
}

func newLogsRoutingTestCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:  logsCmd.Use,
		RunE: runLogsCmd,
	}
	cmd.Flags().BoolVarP(&logsFollow, "follow", "f", false,
		"follow today's log file as new lines are written")
	cmd.Flags().IntVarP(&logsLines, "lines", "n", 50,
		"number of trailing lines to show before following")
	cmd.Flags().StringVar(&logsRunID, "run-id", "",
		"filter to a single run (matches on prefix)")
	cmd.Flags().StringVar(&logsLevel, "level", "",
		"filter by log level: debug, info, warn, error")
	cmd.Flags().StringVar(&logsGrep, "grep", "",
		"substring filter applied to the raw JSON record")
	cmd.Flags().BoolVar(&logsAll, "all", false,
		"read every log file in the logs directory, not just today's")
	cmd.Flags().BoolVar(&logsPath, "path", false,
		"print the log directory path and exit")
	return cmd
}
