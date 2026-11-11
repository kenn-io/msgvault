package cmd

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestRepairAutomationFlagsRoutesThroughDaemon(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal(t, []string{"repair-automation-flags", "--apply", "--batch-size=1"}, req.Args)
	}, `{"type":"stdout","data":"repair complete\n"}`, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")
	cmd := newRepairAutomationFlagsCmd()
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--apply", "--batch-size", "1"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, "repair complete\n", out.String())
}

func TestRepairAutomationFlagsSummaryAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name         string
		apply        bool
		refreshFails bool
		outputFails  bool
		output       string
	}{
		{
			name: "dry run",
			output: "Automation flag repair dry run: scanned=3 would_write=2 bulk=1 automated=1 unreadable=1 remaining=3\n" +
				"Dry run: no rows were modified. Re-run with --apply to write repairs.\n",
		},
		{
			name:   "apply",
			apply:  true,
			output: "Automation flag repair applied: scanned=3 written=2 bulk=1 automated=1 unreadable=1 remaining=1\n",
		},
		{name: "refresh failure", apply: true, refreshFails: true},
		{name: "refresh and output failures", apply: true, refreshFails: true, outputFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
			newListIDRepairArchive(t, cfg)
			var refreshErr error
			if tc.refreshFails {
				_, err := buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
				require.NoError(err)
				refreshErr = errors.New("automation cache export failed")
				buildCacheBeforeMessagesExportHook = func() error { return refreshErr }
				t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })
			}
			cmd := &cobra.Command{}
			cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			var out bytes.Buffer
			cmd.SetOut(&out)
			if tc.outputFails {
				cmd.SetOut(failingSearchWriter{})
			}
			err := runRepairAutomationFlagsLocal(cmd, tc.apply, 1)
			if tc.refreshFails {
				require.ErrorIs(err, refreshErr)
				if tc.outputFails {
					require.ErrorIs(err, io.ErrClosedPipe)
				}
				return
			}
			require.NoError(err)
			assert.Equal(tc.output, out.String())
		})
	}
}
