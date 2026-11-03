package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/docs"
	"go.kenn.io/msgvault/internal/config"
)

func TestQuickstartOffline(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "cli", want: quickstartText},
		{name: "mcp", args: []string{"--mcp"}, want: mcpQuickstart(docs.AgentQuickstart)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if helpTestSubprocess(t) {
				return
			}
			configPath := filepath.Join(t.TempDir(), "invalid.toml")
			require.NoError(t, os.WriteFile(configPath, []byte("invalid = ["), 0o600))
			var output bytes.Buffer
			rootCmd.SetOut(&output)
			rootCmd.SetArgs(append([]string{"--config", configPath, "quickstart"}, tc.args...))
			require.NoError(t, executeRootContext(t.Context(), rootCmd))
			assert.Equal(t, tc.want, output.String())
			entries, err := os.ReadDir(os.Getenv("MSGVAULT_HOME"))
			require.NoError(t, err)
			assert.Empty(t, entries, "quickstart must not create archive or home files")
		})
	}
}

func TestMCPQuickstartNormalizesGuide(t *testing.T) {
	lfDoc := strings.ReplaceAll(docs.AgentQuickstart, "\r\n", "\n")
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{name: "LF checkout", doc: lfDoc},
		{name: "CRLF checkout", doc: strings.ReplaceAll(lfDoc, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := mcpQuickstart(tc.doc)
			assert.True(t, strings.HasPrefix(output, "# msgvault MCP quickstart\n\n"))
			assert.NotContains(t, output, "\r")
			assert.NotContains(t, output, "title:")
			links := regexp.MustCompile(`\]\(([^)\s]+)\)`).FindAllStringSubmatch(output, -1)
			require.NotEmpty(t, links)
			for _, link := range links {
				assert.True(t, strings.HasPrefix(link[1], "https://") || strings.HasPrefix(link[1], "#"), "link must be absolute: %s", link[1])
			}
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
