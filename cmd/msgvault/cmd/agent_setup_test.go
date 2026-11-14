package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentsetup"
)

func TestAgentSetup(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("PATH", t.TempDir())
	opts := agentsetup.Options{Agents: []string{"claude", "codex"}, SetupCommand: "msgvault agent setup", Items: agentSetupItems}
	report, err := agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, report.Rows, 4)
	assert.Equal(t, agentsetup.StateOK, report.Rows[0].State)
	assert.Equal(t, agentsetup.StateUnknown, report.Rows[1].State)
	assert.Equal(t, agentsetup.StateOK, report.Rows[2].State)
	assert.Equal(t, agentsetup.StateUnknown, report.Rows[3].State)
	paths := []string{filepath.Join(home, ".claude", ".claude.json"), filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".claude", "skills", "msgvault-search", "SKILL.md"), filepath.Join(home, ".codex", "skills", "msgvault-search", "SKILL.md")}
	before := make([][]byte, len(paths))
	for i, path := range paths {
		before[i], err = os.ReadFile(path)
		require.NoError(t, err)
	}
	assert.Contains(t, string(before[0]), "msgvault")
	assert.Contains(t, string(before[1]), "msgvault")
	old := time.Unix(1600000000, 0)
	for _, path := range paths[2:] {
		require.NoError(t, os.Chtimes(path, old, old))
	}
	_, err = agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	for i, path := range paths {
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, before[i], after)
		if i >= 2 {
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, old, info.ModTime())
		}
	}
	require.NoError(t, os.WriteFile(paths[2], []byte("custom"), 0o600))
	report, err = agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateModified, report.Rows[0].State)
	assert.Equal(t, "msgvault skills install --force --agent claude", report.Rows[0].NextStep)
	content, err := os.ReadFile(paths[2])
	require.NoError(t, err)
	assert.Equal(t, "custom", string(content))
	var output bytes.Buffer
	require.NoError(t, agentsetup.Print(&output, opts))
	assert.Contains(t, output.String(), "claude")
	assert.Contains(t, output.String(), "codex")
	opts.Agents = []string{"gemini"}
	report, err = agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, report.Rows, 1)
	assert.Equal(t, agentsetup.StateUnknown, report.Rows[0].State)
	cmd, _, err := rootCmd.Find([]string{"agent", "setup"})
	require.NoError(t, err)
	assert.True(t, skipsConfigLoad(cmd))
}

func TestAgentSetupCommand(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("PATH", t.TempDir())
	command := agentsetup.NewCommand(agentsetup.Options{SetupCommand: "msgvault agent setup", Items: agentSetupItems})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--agent", "claude,codex", "--print"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Contains(t, output.String(), "claude mcp add")
	assert.Contains(t, output.String(), "codex mcp add")
	assert.NoDirExists(t, filepath.Join(home, ".claude"))
	assert.NoDirExists(t, filepath.Join(home, ".codex"))
	output.Reset()
	command = agentsetup.NewCommand(agentsetup.Options{SetupCommand: "msgvault agent setup", Items: agentSetupItems})
	command.SetOut(&output)
	command.SetArgs([]string{"--agent", "claude,codex"})
	err := command.ExecuteContext(t.Context())
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	assert.Contains(t, output.String(), "unknown")
	assert.FileExists(t, filepath.Join(home, ".claude", "skills", "msgvault-search", "SKILL.md"))
}

func TestAgentSetupCustomRootsAndScopedRepair(t *testing.T) {
	home := t.TempDir()
	claude := t.TempDir()
	codex := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("PATH", t.TempDir())
	opts := agentsetup.Options{Agents: []string{"claude", "codex"}, SetupCommand: "msgvault agent setup", Items: agentSetupItems}
	report, err := agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOK, report.Rows[0].State)
	assert.Equal(t, agentsetup.StateOK, report.Rows[2].State)
	claudeSkill := filepath.Join(claude, "skills", "msgvault-search", "SKILL.md")
	codexSkill := filepath.Join(codex, "skills", "msgvault-search", "SKILL.md")
	require.FileExists(t, claudeSkill)
	require.FileExists(t, codexSkill)
	require.NoError(t, os.WriteFile(claudeSkill, []byte("claude custom"), 0o600))
	require.NoError(t, os.WriteFile(codexSkill, []byte("codex custom"), 0o600))
	report, err = agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateModified, report.Rows[0].State)
	assert.Equal(t, "msgvault skills install --force --agent claude", report.Rows[0].NextStep)
	var output bytes.Buffer
	require.NoError(t, runSkillsInstall(&output, []string{"claude"}, "", true))
	content, err := os.ReadFile(codexSkill)
	require.NoError(t, err)
	assert.Equal(t, "codex custom", string(content))
	content, err = os.ReadFile(claudeSkill)
	require.NoError(t, err)
	assert.Contains(t, string(content), "generated by msgvault")
}

func TestAgentSetupSelectsInstalledAgents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub requires Unix")
	}
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	setup, err := agentsetup.Run(t.Context(), agentsetup.Options{SetupCommand: "msgvault agent setup", Items: agentSetupItems})
	require.NoError(t, err)
	status, err := checkAgentSetup(t.Context())
	require.NoError(t, err)
	require.Len(t, setup.Rows, 2)
	require.Len(t, status.Rows, 2)
	for i := range setup.Rows {
		assert.Equal(t, setup.Rows[i].Agent, status.Rows[i].Agent)
		assert.Equal(t, setup.Rows[i].Piece, status.Rows[i].Piece)
	}
	assert.NoDirExists(t, filepath.Join(home, ".codex"))
}
