package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelperProcess is a cross-platform subprocess used by the classify tests.
// It re-execs the test binary rather than depending on an external shell (sh),
// which may be absent on minimal Linux images and on Windows.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("GO_HELPER_MODE") {
	case "exit3":
		os.Exit(3)
	case "block":
		select {}
	default:
		os.Exit(0)
	}
}

func helperProcessCommand(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess") //nolint:gosec // os.Args[0] is the test binary; args are fixed test flags.
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "GO_HELPER_MODE="+mode)
	return cmd
}

func TestClassifyDaemonCLIWaitErrExitStatus(t *testing.T) {
	require := require.New(t)

	// A normal non-zero exit yields an *exec.ExitError with Exited() == true.
	waitErr := helperProcessCommand(context.Background(), "exit3").Run()
	require.Error(waitErr, "helper should exit non-zero")
	var exitErr *exec.ExitError
	require.ErrorAs(waitErr, &exitErr, "want *exec.ExitError")

	got := classifyDaemonCLIWaitErr(waitErr, []string{"show-deletion", "0"})
	require.Error(got, "want error")
	require.Equal(cliSubprocessExitSentinel, got.Error(),
		"non-zero exit must map to the sentinel, not a wrapped 'exit status' line")
}

// TestClassifyDaemonCLIWaitErrSignalTerminated verifies a signal-terminated
// subprocess (which also surfaces as *exec.ExitError) stays wrapped with
// context rather than collapsing to the silent sentinel — nothing was streamed
// to the caller for a killed process. Unix-only: Windows lacks signal exits
// (TerminateProcess reports a normal exit code).
func TestClassifyDaemonCLIWaitErrSignalTerminated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-terminated exit semantics are Unix-specific")
	}
	require := require.New(t)

	cmd := helperProcessCommand(context.Background(), "block")
	require.NoError(cmd.Start(), "start blocking helper")
	require.NoError(cmd.Process.Kill(), "kill helper")
	waitErr := cmd.Wait()
	require.Error(waitErr, "killed process should error")
	var exitErr *exec.ExitError
	require.ErrorAs(waitErr, &exitErr, "want *exec.ExitError")
	require.False(exitErr.Exited(), "killed process did not exit normally")

	got := classifyDaemonCLIWaitErr(waitErr, []string{"show-deletion", "0"})
	require.Error(got, "want error")
	require.ErrorContains(got, "CLI subprocess show-deletion 0",
		"signal termination keeps context instead of the silent sentinel")
	require.ErrorIs(got, waitErr, "wraps the original wait error")
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
