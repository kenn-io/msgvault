package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

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
