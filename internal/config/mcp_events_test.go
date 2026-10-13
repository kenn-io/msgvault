package config_test

import (
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/config"
)

func TestMCPEventsConfigDefaultsAndParsedOptions(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	defaults := config.NewDefaultConfig()
	assert.False(defaults.MCP.Events.Enabled)
	assert.Equal("168h", defaults.MCP.Events.Retention)
	assert.Equal([]string{"gmail", "imap", "gcal"}, defaults.MCP.Events.Sources)
	root := t.TempDir()
	parsed, err := config.LoadConfigFile(config.ConfigFile{Exists: true, LogicalPath: filepath.Join(root, "config.toml"), Content: []byte(`[mcp.events]
enabled = true
retention = "24h"
sources = ["imap", "gcal"]
trusted_callbacks = [{origin = "https://receiver.example.net:8443", addresses = ["10.0.0.5"]}]
`)}, root)
	require.NoError(err)
	assert.True(parsed.MCP.Events.Enabled)
	retention, err := parsed.MCP.Events.RetentionDuration()
	require.NoError(err)
	assert.Equal(24*time.Hour, retention)
	assert.Equal([]string{"imap", "gcal"}, parsed.MCP.Events.Sources)
	require.Len(parsed.MCP.Events.TrustedCallbacks, 1)
	assert.Equal("https://receiver.example.net:8443", parsed.MCP.Events.TrustedCallbacks[0].Origin)
	assert.Equal([]string{"10.0.0.5"}, parsed.MCP.Events.TrustedCallbacks[0].Addresses)
}

func TestMCPEventsConfigRejectsUnsafeOrUnboundedOptions(t *testing.T) {
	for _, content := range []string{
		`retention = "0s"`, `retention = "-1h"`, `retention = "169h"`, `retention = "unbounded"`,
		`trusted_callbacks = [{origin = "https://receiver.example.net:9443", addresses = ["10.0.0.5"]}]`,
		`trusted_callbacks = [{origin = "https://receiver.example.net/hook", addresses = ["10.0.0.5"]}]`,
		`trusted_callbacks = [{origin = "https://receiver.example.net", addresses = []}]`,
		`trusted_callbacks = [{origin = "https://receiver.example.net", addresses = ["127.0.0.1"]}]`,
	} {
		t.Run(content, func(t *testing.T) {
			root := t.TempDir()
			_, err := config.LoadConfigFile(config.ConfigFile{Exists: true, LogicalPath: filepath.Join(root, "config.toml"), Content: []byte("[mcp.events]\n" + content + "\n")}, root)
			Require.Error(t, err)
		})
	}
}
