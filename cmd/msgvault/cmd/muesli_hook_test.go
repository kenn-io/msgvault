package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMuesliHookEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	good := `{"schemaVersion":1,"event":"meeting.completed","kind":"meeting","id":42,"completedAt":"2026-09-01T15:00:00Z"}`
	id, err := decodeMuesliHook(strings.NewReader(good))
	require.NoError(err)
	assert.Equal(int64(42), id)
	for _, body := range []string{`{}`, good + `{}`, strings.Replace(good, `"id":42`, `"id":0`, 1), strings.Replace(good, `"schemaVersion":1`, `"schemaVersion":2`, 1), strings.Replace(good, "meeting.completed", "meeting.deleted", 1), strings.Replace(good, `"completedAt":`, `"unknown":"secret","completedAt":`, 1), strings.Repeat(" ", 4097)} {
		_, err := decodeMuesliHook(strings.NewReader(body))
		require.Error(err)
		assert.NotContains(err.Error(), "secret")
	}
}

func TestMuesliHookInstallerRefusesOverwrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path, err := installMuesliHook(dir)
	require.NoError(err)
	assert.Equal("msgvault-muesli-hook", filepath.Base(path))
	executable, err := os.Executable()
	require.NoError(err)
	executable, err = filepath.EvalSymlinks(executable)
	require.NoError(err)
	target, err := os.Readlink(path)
	require.NoError(err)
	assert.Equal(executable, target)
	_, err = installMuesliHook(dir)
	require.Error(err)
	target, err = os.Readlink(path)
	require.NoError(err)
	assert.Equal(executable, target)
}

func TestMuesliHookBuiltArtifactDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	binary := filepath.Join("..", "..", "..", "msgvault")
	if _, err := os.Stat(binary); errors.Is(err, os.ErrNotExist) {
		// CI checkouts may not have a worktree artifact. Build the real CLI
		// so the executable dispatch regression always runs there too.
		repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
		require.NoError(err)
		binary = filepath.Join(t.TempDir(), "msgvault")
		build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5 sqlite_vec", "-o", binary, "./cmd/msgvault")
		build.Dir = repoRoot
		output, err := build.CombinedOutput()
		require.NoError(err, "build real msgvault CLI: %s", output)
	} else {
		require.NoError(err)
	}
	binary, err := filepath.Abs(binary)
	require.NoError(err)
	alias := filepath.Join(t.TempDir(), "msgvault-muesli-hook")
	require.NoError(os.Symlink(binary, alias))
	command := exec.CommandContext(t.Context(), alias, "serve", "--help")
	command.Env = append(os.Environ(), "MSGVAULT_HOME="+t.TempDir())
	output, err := command.CombinedOutput()
	require.NoError(err, string(output))
	assert.Contains(string(output), "Run msgvault as a long-running daemon")
	command = exec.CommandContext(t.Context(), alias)
	command.Env = append(os.Environ(), "MSGVAULT_HOME="+t.TempDir())
	command.Stdin = strings.NewReader(`{}`)
	output, err = command.CombinedOutput()
	require.Error(err)
	assert.Contains(string(output), "invalid Muesli completion event")
}
