package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestRepairAutomationFlagsPrintsSummary(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
	newListIDRepairArchive(t, cfg)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	cmd.SetOut(&out)
	require.NoError(t, runRepairAutomationFlagsLocal(cmd, false, 1))
	assert.Contains(t, out.String(), "Automation flag repair dry run: scanned=3 would_write=2 bulk=1 automated=1 unreadable=1 remaining=3\n")
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
