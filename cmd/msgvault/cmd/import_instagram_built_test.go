package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This opt-in test exercises the packaged command, real daemon CLI runner,
// archive writes and cache refresh using only invented data.
func TestInstagramBuiltCLI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	binary := os.Getenv("MSGVAULT_E2E_BINARY")
	if binary == "" {
		t.Skip("set MSGVAULT_E2E_BINARY to the make build binary")
	}
	binary, err := filepath.Abs(binary)
	require.NoError(err)
	home := t.TempDir()
	execute := func(ctx context.Context, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, binary, append([]string{"--home", home, "--local"}, args...)...) //nolint:gosec // The opt-in operator supplies the built binary; arguments are passed without a shell.
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "TMPDIR=" + home, "NO_COLOR=1"}
		return command.CombinedOutput()
	}
	t.Cleanup(func() {
		// Test context is canceled before cleanup; the fixture daemon still needs to stop.
		output, err := execute(context.WithoutCancel(t.Context()), "daemon", "stop")
		assert.NoError(err, "stop fixture daemon: %s", output)
	})
	run := func(args ...string) []byte {
		t.Helper()
		output, err := execute(t.Context(), args...)
		require.NoError(err, "%v: %s", args, output)
		return output
	}
	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/instagram")
	require.NoError(err)
	first := string(run("import-instagram", "--account", "orbit.example", "--self-name", "unmatched", "--limit", "2", fixture))
	assert.Contains(first, "No sender exactly matched --self-name")
	assert.Contains(first, "Import paused at --limit")
}
