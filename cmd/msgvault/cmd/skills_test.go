package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/skills"
)

// setTestHome points os.UserHomeDir() at home for the duration of the
// test. HOME is read on Unix; USERPROFILE is read on Windows. Setting
// both keeps these tests platform-independent and prevents accidental
// writes to (or deletion of) a real user's home directory when run on
// Windows without USERPROFILE set.
func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func TestRunSkillsInstall_DetectsAgents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	setTestHome(t, home)
	require.NoError(os.Mkdir(filepath.Join(home, ".claude"), 0o755))
	require.NoError(os.Mkdir(filepath.Join(home, ".codex"), 0o755))

	var out bytes.Buffer
	require.NoError(runSkillsInstall(&out, nil, "", false))

	for _, agent := range []string{".claude", ".codex"} {
		path := filepath.Join(
			home, agent, "skills", "msgvault-search", "SKILL.md")
		content, err := os.ReadFile(path)
		require.NoError(err, "skill must be installed for %s", agent)
		assert.Contains(string(content), skills.Marker)
	}
	assert.Contains(out.String(), "installed")
}

func TestRunSkillsInstall_NoAgentsDetected(t *testing.T) {
	setTestHome(t, t.TempDir())
	var out bytes.Buffer
	err := runSkillsInstall(&out, nil, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dir")
}

func TestRunSkillsInstall_ExplicitDir(t *testing.T) {
	setTestHome(t, t.TempDir()) // no agent dirs; --dir must not need them
	dir := filepath.Join(t.TempDir(), "custom-skills")
	var out bytes.Buffer
	require.NoError(t, runSkillsInstall(&out, nil, dir, false))
	assert.FileExists(t,
		filepath.Join(dir, "msgvault-analytics", "SKILL.md"))
}

func TestRunSkillsInstall_ReportsSkipped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	setTestHome(t, home)
	require.NoError(os.Mkdir(filepath.Join(home, ".claude"), 0o755))

	var out bytes.Buffer
	require.NoError(runSkillsInstall(&out, nil, "", false))
	edited := filepath.Join(
		home, ".claude", "skills", "msgvault-search", "SKILL.md")
	require.NoError(os.WriteFile(edited, []byte("mine\n"), 0o644))

	out.Reset()
	require.NoError(runSkillsInstall(&out, nil, "", false))
	assert.Contains(out.String(), "skipped")
	assert.Contains(out.String(), "--force")
}

func TestRunSkillsUninstall(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	setTestHome(t, home)
	require.NoError(os.Mkdir(filepath.Join(home, ".codex"), 0o755))

	var out bytes.Buffer
	require.NoError(runSkillsInstall(&out, nil, "", false))
	require.NoError(runSkillsUninstall(&out, nil, ""))
	assert.NoDirExists(
		filepath.Join(home, ".codex", "skills", "msgvault-search"))
}

func TestSkillsCommandSkipsConfigLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	installCmd, _, err := rootCmd.Find([]string{"skills", "install"})
	require.NoError(err)
	require.Equal("install", installCmd.Name())
	assert.True(skipsConfigLoad(installCmd),
		"skills install must not load config or touch msgvault home")

	uninstallCmd, _, err := rootCmd.Find([]string{"skills", "uninstall"})
	require.NoError(err)
	assert.True(skipsConfigLoad(uninstallCmd))

	searchCmd, _, err := rootCmd.Find([]string{"search"})
	require.NoError(err)
	assert.False(skipsConfigLoad(searchCmd))
}
