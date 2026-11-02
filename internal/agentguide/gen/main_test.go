package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcp"
)

func TestPublisherChecksCommittedArtifacts(t *testing.T) {
	// Fork PRs execute the main-pinned CI workflow, so a new Makefile check in
	// this series is not yet run there. Also protect direct `go test` users by
	// checking the real committed artifacts through the production publisher.
	require.NoError(t, publishGuide("../quickstart.md", "../../../docs/guides/agent-quickstart.md", true))
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
