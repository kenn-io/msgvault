package cmd

import (
	"bytes"
	"encoding/json"
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

func TestBriefingTargetsDirWhenGiven(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	project := filepath.Join(t.TempDir(), "project [example]")
	var out bytes.Buffer
	command := newSkillsInstallCommand()
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--dir", project, "--briefing"})
	require.NoError(t, command.Execute())
	for _, target := range []string{".claude/settings.json", ".codex/hooks.json"} {
		groups := readBriefingGroups(t, filepath.Join(project, filepath.FromSlash(target)))
		require.Len(t, groups, 1)
		assert.Equal(t, []briefingTestHandler{{Type: "command", Command: "msgvault timeline --brief"}}, groups[0].Hooks)
		assert.NoFileExists(t, filepath.Join(home, filepath.FromSlash(target)))
	}
	assert.FileExists(t, filepath.Join(project, "msgvault-timeline", "SKILL.md"))
	assert.Contains(t, out.String(), "/hooks", "Codex hooks retain their normal trust review")

	uninstall := newSkillsUninstallCommand()
	uninstall.SetOut(&out)
	uninstall.SetErr(&out)
	uninstall.SetArgs([]string{"--dir", project})
	require.NoError(t, uninstall.Execute())
	for _, target := range []string{".claude/settings.json", ".codex/hooks.json"} {
		data, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(target)))
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(data))
	}
}

func TestBriefingTargetsSelectedAgent(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			setTestHome(t, home)
			for _, name := range []string{".claude", ".codex"} {
				require.NoError(t, os.Mkdir(filepath.Join(home, name), 0o700))
			}
			var out bytes.Buffer
			command := newSkillsInstallCommand()
			command.SetOut(&out)
			command.SetErr(&out)
			command.SetArgs([]string{"--agent", agent, "--briefing"})
			require.NoError(t, command.Execute())
			target, other := ".claude/settings.json", ".codex/hooks.json"
			if agent == "codex" {
				target, other = other, target
			}
			path := filepath.Join(home, filepath.FromSlash(target))
			groups := readBriefingGroups(t, path)
			require.Len(t, groups, 1)
			assert.Equal(t, []briefingTestHandler{{Type: "command", Command: "msgvault timeline --brief"}}, groups[0].Hooks)
			assert.NoFileExists(t, filepath.Join(home, filepath.FromSlash(other)))
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			// A normal skill refresh must leave the opt-in hook unchanged.
			require.NoError(t, runSkillsInstall(&out, []string{agent}, "", false))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			require.NoError(t, runSkillsUninstall(&out, []string{agent}, ""))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.JSONEq(t, `{}`, string(data))
		})
	}
}

func TestBriefingIsOptIn(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	for _, agent := range []string{".claude", ".codex"} {
		require.NoError(t, os.Mkdir(filepath.Join(home, agent), 0o700))
	}
	var out bytes.Buffer
	command := newSkillsInstallCommand()
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs(nil)
	require.NoError(t, command.Execute())
	assert.NoFileExists(t, filepath.Join(home, ".claude", "settings.json"))
	assert.NoFileExists(t, filepath.Join(home, ".codex", "hooks.json"))
	require.NoError(t, runSkillsUninstall(&out, nil, ""))
	assert.NoFileExists(t, filepath.Join(home, ".claude", "settings.json"))
	assert.NoFileExists(t, filepath.Join(home, ".codex", "hooks.json"))
}

type briefingTestHandler struct {
	Type string `json:"type"`
	Command string `json:"command"`
}

type briefingTestGroup struct {
	Hooks []briefingTestHandler `json:"hooks"`
}

func readBriefingGroups(t *testing.T, path string) []briefingTestGroup {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var got struct {
		Hooks struct {
			SessionStart []briefingTestGroup `json:"SessionStart"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &got))
	return got.Hooks.SessionStart
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
