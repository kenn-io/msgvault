package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentguide"
	"go.kenn.io/msgvault/internal/config"
)

func TestQuickstartHelp(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"quickstart", "--help"})
	require.NoError(t, executeRootContext(t.Context(), rootCmd))
	assert.Regexp(t, `(?m)^\s+--mcp\s`, output.String(), "the flag must appear in the rendered flag list")
}

func TestQuickstartOffline(t *testing.T) {
	requirements := require.New(t)
	if mode := os.Getenv("MSGVAULT_QUICKSTART_TEST"); mode != "" {
		path := filepath.Join(t.TempDir(), "invalid.toml")
		requirements.NoError(os.WriteFile(path, []byte("[invalid TOML"), 0o600))
		args := []string{"--config", path, "quickstart"}
		want := quickstartText
		if mode == "mcp" {
			args = append(args, "--mcp")
			want = agentguide.Quickstart()
		}
		var output bytes.Buffer
		rootCmd.SetOut(&output)
		rootCmd.SetErr(&output)
		rootCmd.SetArgs(args)
		requirements.NoError(rootCmd.Execute(), output.String())
		assert.Equal(t, want, output.String())
		return
	}

	executable, err := os.Executable()
	requirements.NoError(err)
	for _, mode := range []string{"cli", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			home := t.TempDir()
			child := exec.CommandContext(t.Context(), executable, "-test.run=^TestQuickstartOffline$", "-test.count=1")
			for _, variable := range os.Environ() {
				key, _, _ := strings.Cut(variable, "=")
				switch key {
				case "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "MSGVAULT_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "MSGVAULT_QUICKSTART_TEST":
					continue
				}
				child.Env = append(child.Env, variable)
			}
			child.Env = append(child.Env, "HOME="+home, "XDG_CONFIG_HOME="+home, "XDG_DATA_HOME="+home, "MSGVAULT_HOME="+home, "MSGVAULT_QUICKSTART_TEST="+mode)
			output, err := child.CombinedOutput()
			requirements.NoError(err, "%s", output)
		})
	}
}

func TestQuickstartConfigDoesNotEnableRemoteDeletion(t *testing.T) {
	lfDoc := strings.ReplaceAll(quickstartText, "\r\n", "\n")
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{name: "LF checkout", doc: lfDoc},
		{name: "CRLF checkout", doc: strings.ReplaceAll(lfDoc, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			// Embedded files preserve the checkout's exact bytes. Normalize the
			// documentation before locating its Markdown fences so Windows and
			// Unix checkouts exercise the same real config parser path.
			doc := strings.ReplaceAll(tc.doc, "\r\n", "\n")
			_, fenced, found := strings.Cut(doc, "### Configuration")
			require.True(found, "quickstart must contain a Configuration section")
			_, snippet, found := strings.Cut(fenced, "```toml\n")
			require.True(found, "Configuration section must contain a TOML fence")
			snippet, _, found = strings.Cut(snippet, "\n```")
			require.True(found, "Configuration TOML fence must be closed")

			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(snippet), 0o600))
			cfg, err := config.Load(path, "")
			require.NoError(err)
			assert.False(t, cfg.Deletion.RemoteEnabled,
				"copying the quickstart config must not grant remote deletion consent")
		})
	}
}
