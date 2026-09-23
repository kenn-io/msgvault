//go:build linux

package peoplesweep

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexLoginAuthCommitRequiresPrivateFileAndKeepsDedicatedHome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		authMode  os.FileMode
		symlink   bool
		preexists bool
		wantError string
	}{
		{name: "valid", authMode: 0o600},
		{name: "public file", authMode: 0o644, wantError: "private regular file"},
		{name: "symlink", authMode: 0o600, symlink: true, wantError: "private regular file"},
		{name: "invalid existing auth", authMode: 0o600, preexists: true, wantError: "unsafe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertChecks := assert.New(t)
			requireChecks := require.New(t)
			workRoot := t.TempDir()
			requireChecks.NoError(os.Chmod(workRoot, 0o700))
			stagedDir := filepath.Join(workRoot, ".codex")
			requireChecks.NoError(os.Mkdir(stagedDir, 0o700))
			authFile := filepath.Join(stagedDir, "auth.json")
			if tc.symlink {
				target := filepath.Join(workRoot, "unrelated.json")
				requireChecks.NoError(os.WriteFile(target, syntheticCodexProbeAuth("user-one", "workspace-one", "refresh"), 0o600))
				requireChecks.NoError(os.Symlink(target, authFile))
			} else {
				requireChecks.NoError(os.WriteFile(authFile, syntheticCodexProbeAuth("user-one", "workspace-one", "refresh"), tc.authMode))
			}
			authHome := t.TempDir()
			requireChecks.NoError(os.Chmod(authHome, 0o700))
			if tc.preexists {
				requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), []byte("EXISTING"), 0o600))
			}
			process := &codexOwnedProcess{workRoot: workRoot}
			err := process.CommitLoginAuth(authHome)
			if tc.wantError != "" {
				requireChecks.ErrorContains(err, tc.wantError)
				if tc.preexists {
					contents, readErr := os.ReadFile(filepath.Join(authHome, "auth.json"))
					requireChecks.NoError(readErr)
					assertChecks.Equal("EXISTING", string(contents))
				} else {
					assertChecks.NoFileExists(filepath.Join(authHome, "auth.json"))
				}
				return
			}
			requireChecks.NoError(err)
			requireChecks.NoError(process.cleanup())
			assertChecks.NoDirExists(workRoot)
			contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			assertChecks.Equal(syntheticCodexProbeAuth("user-one", "workspace-one", "refresh"), contents)
			info, err := os.Stat(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			assertChecks.Equal(os.FileMode(0o600), info.Mode().Perm())
		})
	}
}

func TestCodexLoginReplacesPrivateAuthAfterValidAccountSwitch(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(workRoot, 0o700))
	stagedDir := filepath.Join(workRoot, ".codex")
	requireChecks.NoError(os.Mkdir(stagedDir, 0o700))
	newAuth := syntheticCodexProbeAuth("user-two", "workspace-two", "new-refresh")
	requireChecks.NoError(os.WriteFile(filepath.Join(stagedDir, "auth.json"), newAuth, 0o600))
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	oldAuth := syntheticCodexProbeAuth("user-one", "workspace-one", "old-refresh")
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), oldAuth, 0o600))
	process := &codexOwnedProcess{workRoot: workRoot}
	requireChecks.NoError(process.CommitLoginAuth(authHome))
	contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal(newAuth, contents)
	info, err := os.Lstat(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal(os.FileMode(0o600), info.Mode().Perm())
}

func TestCodexLoginRejectedReplacementKeepsOldAuth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate []byte
		oldMode   os.FileMode
	}{
		{name: "invalid candidate identity", candidate: []byte(`{"synthetic":true}`), oldMode: 0o600},
		{name: "public old auth", candidate: syntheticCodexProbeAuth("user-two", "workspace-two", "new"), oldMode: 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireChecks := require.New(t)
			workRoot := t.TempDir()
			requireChecks.NoError(os.Chmod(workRoot, 0o700))
			requireChecks.NoError(os.Mkdir(filepath.Join(workRoot, ".codex"), 0o700))
			requireChecks.NoError(os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), tc.candidate, 0o600))
			authHome := t.TempDir()
			requireChecks.NoError(os.Chmod(authHome, 0o700))
			old := syntheticCodexProbeAuth("user-one", "workspace-one", "old")
			requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), old, tc.oldMode))
			requireChecks.Error((&codexOwnedProcess{workRoot: workRoot}).CommitLoginAuth(authHome))
			contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			assert.Equal(t, old, contents)
		})
	}
}
