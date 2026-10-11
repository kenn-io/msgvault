package config_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"path/filepath"
	"testing"
)

func TestMCPAllEligibleSelectionParsing(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	defaults := config.NewDefaultConfig()
	assert.False(defaults.MCP.Events.Enabled)
	assert.Equal([]string{"gmail", "imap", "gcal"}, defaults.MCP.Events.Sources)
	root := t.TempDir()
	parse := func(sources string) (*config.Config, error) {
		return config.LoadConfigFile(config.ConfigFile{Exists: true, LogicalPath: filepath.Join(root, "config.toml"), Content: []byte("[mcp.events]\nenabled = true\nsources = " + sources + "\n")}, root)
	}
	all, err := parse(`["*"]`)
	require.NoError(err)
	assert.Equal([]string{"*"}, all.MCP.Events.Sources)
	for _, sources := range []string{`["*", "gmail"]`, `["*", "*"]`} {
		_, err := parse(sources)
		require.Error(err, "the all-eligible selection must be unambiguous")
	}
}
