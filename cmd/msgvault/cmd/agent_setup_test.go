package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentsetup"
)

func TestAgentSetup(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("PATH", t.TempDir())
	opts := agentsetup.Options{Agents: []string{"gemini"}, SetupCommand: "msgvault agent setup", Items: agentSetupItems}
	report, err := agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, report.Rows, 1)
	assert.Equal(t, agentsetup.StateUnknown, report.Rows[0].State)
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
	_, err := agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	claudeSkill := filepath.Join(claude, "skills", "msgvault-search", "SKILL.md")
	codexSkill := filepath.Join(codex, "skills", "msgvault-search", "SKILL.md")
	require.FileExists(t, claudeSkill)
	require.FileExists(t, codexSkill)
	require.NoError(t, os.WriteFile(claudeSkill, []byte("claude custom"), 0o600))
	require.NoError(t, os.WriteFile(codexSkill, []byte("codex custom"), 0o600))
	report, err := agentsetup.Run(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateModified, report.Rows[0].State)
	assert.Contains(t, report.Rows[0].Reason, "has no msgvault marker")
	assert.Equal(t, "msgvault skills install --force --agent claude", report.Rows[0].NextStep)
	require.NoError(t, runSkillsInstall(&bytes.Buffer{}, []string{"claude"}, "", true))
	content, err := os.ReadFile(codexSkill)
	require.NoError(t, err)
	assert.Equal(t, "codex custom", string(content))
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
	status, err := checkAgentSetup(t.Context())
	require.NoError(t, err)
	require.Len(t, status.Rows, 2)
	assert.NoDirExists(t, filepath.Join(home, ".claude"))
	assert.NoDirExists(t, filepath.Join(home, ".codex"))
}
