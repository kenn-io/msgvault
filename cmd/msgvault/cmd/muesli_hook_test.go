package cmd

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestMuesliHookInstallerReplacesEarlierLink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	name := "msgvault-muesli-hook"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	hook := filepath.Join(dir, name)
	// A package upgrade removed the version an earlier install pointed at.
	require.NoError(os.Symlink(filepath.Join(t.TempDir(), "removed-version", "msgvault"), hook))
	path, err := installMuesliHook(dir)
	require.NoError(err)
	assert.Equal(hook, path)
	installed, err := os.Stat(path)
	require.NoError(err, "the hook must resolve after reinstalling")
	executable, err := os.Executable()
	require.NoError(err)
	running, err := os.Stat(executable)
	require.NoError(err)
	assert.True(os.SameFile(running, installed))
}

func TestMuesliHookInstallerRefusesToReplaceFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	name := "msgvault-muesli-hook"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	hook := filepath.Join(dir, name)
	require.NoError(os.WriteFile(hook, []byte("synthetic user script"), 0o700))
	_, err := installMuesliHook(dir)
	require.Error(err)
	data, err := os.ReadFile(hook)
	require.NoError(err)
	assert.Equal("synthetic user script", string(data))
}

func TestMuesliHookBuiltArtifactDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binary := filepath.Join("..", "..", "..", "msgvault"+suffix)
	if _, err := os.Stat(binary); errors.Is(err, os.ErrNotExist) {
		// CI checkouts may not have a worktree artifact. Build the real CLI
		// so the executable dispatch regression always runs there too.
		repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
		require.NoError(err)
		binary = filepath.Join(t.TempDir(), "msgvault"+suffix)
		build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5 sqlite_vec", "-o", binary, "./cmd/msgvault")
		build.Dir = repoRoot
		output, err := build.CombinedOutput()
		require.NoError(err, "build real msgvault CLI: %s", output)
	} else {
		require.NoError(err)
	}
	binary, err := filepath.Abs(binary)
	require.NoError(err)
	alias := filepath.Join(t.TempDir(), "msgvault-muesli-hook"+suffix)
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
	t.Run("installed executable suffix", func(t *testing.T) {
		testMuesliHookInstallerExecutableSuffix(t, binary)
	})
	t.Run("installed through a stable symlink", func(t *testing.T) {
		testMuesliHookInstallerKeepsInvokedPath(t, binary, suffix)
	})
}

// A package manager's bin entry links to a versioned file that an upgrade
// removes; the hook must keep pointing at the bin entry.
func testMuesliHookInstallerKeepsInvokedPath(t *testing.T, binary, suffix string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	stable := filepath.Join(t.TempDir(), "msgvault"+suffix)
	require.NoError(os.Symlink(binary, stable))
	dir := t.TempDir()
	command := exec.CommandContext(t.Context(), stable, "muesli-hook", "--install", dir)
	command.Env = append(os.Environ(), "MSGVAULT_HOME="+t.TempDir())
	output, err := command.Output()
	require.NoError(err, string(output))
	hook := strings.TrimSpace(string(output))
	target, err := os.Readlink(hook)
	require.NoError(err)
	assert.Equal(stable, target)

	// Reinstalling by running the hook name must not link the hook to itself,
	// even when the install directory is named through a symlinked alias.
	alias := filepath.Join(t.TempDir(), "launchers")
	require.NoError(os.Symlink(dir, alias))
	aliasHook := filepath.Join(alias, filepath.Base(hook))
	for _, reinstall := range []struct{ invoked, installDir string }{
		{hook, dir}, {hook, alias}, {aliasHook, dir},
	} {
		command = exec.CommandContext(t.Context(), reinstall.invoked, "muesli-hook", "--install", reinstall.installDir)
		command.Env = append(os.Environ(), "MSGVAULT_HOME="+t.TempDir())
		output, err = command.Output()
		require.NoError(err, string(output))
		target, err = os.Readlink(hook)
		require.NoError(err)
		assert.Equal(stable, target, "reinstall via %s into %s", reinstall.invoked, reinstall.installDir)
	}
}

func testMuesliHookInstallerExecutableSuffix(t *testing.T, binary string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	executable := filepath.Join(t.TempDir(), "msgvault.exe")
	src, err := os.Open(binary)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(src.Close()) })
	dst, err := os.OpenFile(executable, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	require.NoError(err)
	_, err = io.Copy(dst, src)
	require.NoError(err)
	require.NoError(dst.Close())
	dir := t.TempDir()
	command := exec.CommandContext(t.Context(), executable, "muesli-hook", "--install", dir)
	command.Env = append(os.Environ(), "MSGVAULT_HOME="+t.TempDir())
	output, err := command.Output()
	require.NoError(err, string(output))
	alias := strings.TrimSpace(string(output))
	assert.Equal(filepath.Join(dir, "msgvault-muesli-hook.exe"), alias)
	command = exec.CommandContext(t.Context(), alias)
	command.Env = append(os.Environ(), "MSGVAULT_HOME="+t.TempDir())
	command.Stdin = strings.NewReader(`{}`)
	output, err = command.CombinedOutput()
	require.Error(err)
	assert.Contains(string(output), "invalid Muesli completion event")
}
