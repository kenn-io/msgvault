package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentguide"
)

func TestAgentInstructionsOffline(t *testing.T) {
	for _, kind := range []string{"print", "write", "native quickstart", "conflicting quickstart flags"} {
		t.Run(kind, func(t *testing.T) {
			if helpTestSubprocess(t) {
				return
			}
			dir := t.TempDir()
			invalidConfig := filepath.Join(dir, "invalid.toml")
			require.NoError(t, os.WriteFile(invalidConfig, []byte("invalid = ["), 0o600))
			path := filepath.Join(dir, "AGENTS.md")
			args := []string{"--config", invalidConfig, "agent-instructions"}
			want := agentguide.ManagedBlock() + "\n"
			switch kind {
			case "write":
				require.NoError(t, os.WriteFile(path, []byte("# Existing project\n"), 0o600))
				args = append(args, "--write", path)
				want = "Updated " + path + "\n"
			case "native quickstart", "conflicting quickstart flags":
				args = []string{"--config", invalidConfig, "quickstart", "--mcp-only"}
				want = agentguide.QuickstartMarkdown()
				if kind == "conflicting quickstart flags" {
					args = append(args, "--mcp")
				}
			}
			var output bytes.Buffer
			rootCmd.SetOut(&output)
			rootCmd.SetErr(&output)
			rootCmd.SetArgs(args)
			err := executeRootContext(t.Context(), rootCmd)
			if kind == "conflicting quickstart flags" {
				require.ErrorContains(t, err, "mcp")
			} else {
				require.NoError(t, err)
				assert.Equal(t, want, output.String())
			}
			if kind == "write" {
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, "# Existing project\n\n"+agentguide.ManagedBlock()+"\n", string(content))
			}
			entries, err := os.ReadDir(os.Getenv("MSGVAULT_HOME"))
			require.NoError(t, err)
			assert.Empty(t, entries, "instruction generation must not initialize config or an archive")
		})
	}
}
