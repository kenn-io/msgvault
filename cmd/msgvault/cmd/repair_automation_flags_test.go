package cmd

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRepairAutomationFlagsPrintsSummary(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	require.NoError(t, st.Close())
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	cmd.SetOut(&out)
	require.NoError(t, runRepairAutomationFlagsLocal(cmd, false, 1))
	assert.Regexp(t, `Automation flag repair dry run: scanned=[0-9]+ would_write=[0-9]+ bulk=[0-9]+ automated=[0-9]+ unreadable=[0-9]+ remaining=[0-9]+\n`, out.String())
	assert.Contains(t, out.String(), "Dry run: no rows were modified.")
}

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

func TestRepairAutomationFlagsRefreshAndOutputFailure(t *testing.T) {
	for _, outputFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(outputFailure), func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
			newListIDRepairArchive(t, cfg)
			_, err := buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
			require.NoError(err)
			sentinel := errors.New("automation cache export failed")
			buildCacheBeforeMessagesExportHook = func() error { return sentinel }
			t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })
			cmd := &cobra.Command{}
			cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			var out bytes.Buffer
			cmd.SetOut(&out)
			if outputFailure {
				cmd.SetOut(failingSearchWriter{})
			}
			err = runRepairAutomationFlagsLocal(cmd, true, 1)
			require.ErrorIs(err, sentinel)
			if outputFailure {
				require.ErrorIs(err, io.ErrClosedPipe)
			}
		})
	}
}
