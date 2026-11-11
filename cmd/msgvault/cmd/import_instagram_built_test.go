package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This opt-in test exercises the packaged command, real daemon CLI runner,
// archive writes, cache refresh, search and replay using only invented data.
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
	// Machine-readable search output is stdout only; capture stderr separately.
	command := exec.CommandContext(t.Context(), binary, "--home", home, "--local", "search", "telescope", "--message-type", "instagram", "--json") //nolint:gosec // The opt-in operator supplies the built binary.
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "TMPDIR=" + home, "NO_COLOR=1"}
	output, err := command.Output()
	require.NoError(err)
	var results []struct {
		SourceMessageID string `json:"source_message_id"`
		Snippet         string `json:"snippet"`
	}
	require.NoError(json.Unmarshal(output, &results))
	require.Len(results, 1)
	assert.Contains(results[0].SourceMessageID, "ig:v1:")
	assert.Contains(results[0].Snippet, "telescope")
}
