package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcp"
)

func TestToolTableIsCurrent(t *testing.T) {
	source, err := os.ReadFile("../../../docs/guides/agent-quickstart.md")
	require.NoError(t, err)
	generated, err := updateToolTable(source, mcp.ToolSummaries())
	require.NoError(t, err)
	assert.Equal(t, strings.ReplaceAll(string(source), "\r\n", "\n"), string(generated), "run make agent-guide")
}

func TestToolTableRendering(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	tools := []mcp.ToolSummary{
		{Name: "write_fixture", Security: "write", Flag: "--allow-fixture", GrantPermission: "owner-only"},
		{Name: "read_fixture", Security: "read", GrantPermission: "message.read"},
	}
	source := []byte("Before\r\n" + beginTools + "\r\nstale\r\n" + endTools + "\r\nAfter\r\n")
	got, err := updateToolTable(source, tools)
	requirements.NoError(err)
	slices.Reverse(tools)
	again, err := updateToolTable(got, tools)
	requirements.NoError(err)
	assertions.Equal(got, again, "generation must be deterministic and idempotent")
	assertions.Contains(string(got), "Before\n"+beginTools)
	assertions.Contains(string(got), endTools+"\nAfter\n")
	assertions.Contains(string(got), "| `read_fixture` | read | — | message.read |")
	assertions.Contains(string(got), "| `write_fixture` | write | `--allow-fixture` | owner-only |")
	assertions.Equal("first\\|second next&#96;", markdownCell("first|second\nnext`"))
	for _, invalid := range []string{"", endTools + beginTools, beginTools + beginTools + endTools} {
		_, err := updateToolTable([]byte(invalid), tools)
		requirements.Error(err)
	}
}

func TestSyncFileCheckDoesNotWrite(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "guide.md")
	requirements.NoError(os.WriteFile(path, []byte("old\n"), 0o600))
	requirements.ErrorContains(syncFile(path, []byte("new\n"), true), "is stale")
	current, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal("old\n", string(current))
	requirements.NoError(syncFile(path, []byte("new\n"), false))
	requirements.NoError(syncFile(path, []byte("new\n"), true))
	requirements.NoError(os.WriteFile(path, []byte("new\r\n"), 0o600))
	assertions.NoError(syncFile(path, []byte("new\n"), true))
}
