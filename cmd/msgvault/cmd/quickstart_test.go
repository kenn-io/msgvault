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
	require := require.New(t)
	_, fenced, found := strings.Cut(quickstartText, "### Configuration")
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
}
