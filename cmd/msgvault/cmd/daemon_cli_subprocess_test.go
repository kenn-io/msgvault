package cmd

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyDaemonCLIWaitErrExitStatus(t *testing.T) {
	require := require.New(t)

	// A real non-zero exit yields an *exec.ExitError.
	waitErr := exec.Command("sh", "-c", "exit 3").Run()
	require.Error(waitErr, "sh should exit non-zero")
	var exitErr *exec.ExitError
	require.ErrorAs(waitErr, &exitErr, "want *exec.ExitError")

	got := classifyDaemonCLIWaitErr(waitErr, []string{"show-deletion", "0"})
	require.Error(got, "want error")
	require.Equal(cliSubprocessExitSentinel, got.Error(),
		"non-zero exit must map to the sentinel, not a wrapped 'exit status' line")
}

func TestClassifyDaemonCLIWaitErrOtherFailure(t *testing.T) {
	require := require.New(t)

	base := errors.New("fork/exec: permission denied")
	got := classifyDaemonCLIWaitErr(base, []string{"logs"})
	require.Error(got, "want error")
	require.ErrorContains(got, "CLI subprocess logs", "non-exit failures keep context")
	require.ErrorIs(got, base, "wraps the original error")
}

func TestClassifyDaemonCLIWaitErrNil(t *testing.T) {
	require.NoError(t, classifyDaemonCLIWaitErr(nil, []string{"logs"}), "nil stays nil")
}

func TestDaemonCLIChildEnvAppliesAllowlistedEnvOverrides(t *testing.T) {
	got := daemonCLIChildEnv(
		[]string{
			"PATH=/usr/bin",
			daemonCLISubprocessEnv + "=old",
			"MSGVAULT_IMAP_PASSWORD=old-secret",
		},
		123,
		map[string]string{"MSGVAULT_IMAP_PASSWORD": "new-secret"},
	)

	assert.Equal(t, []string{
		"PATH=/usr/bin",
		daemonCLISubprocessEnv + "=" + strconv.Itoa(123),
		"MSGVAULT_IMAP_PASSWORD=new-secret",
	}, got)
}

func TestNewDaemonCLISubprocessCommandAppliesWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()

	cmd, err := newDaemonCLISubprocessCommand(context.Background(), []string{"version"}, nil, cwd)

	require.NoError(t, err, "newDaemonCLISubprocessCommand")
	assert.Equal(t, cwd, cmd.Dir, "working directory")
}
