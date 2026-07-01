package cmd

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
