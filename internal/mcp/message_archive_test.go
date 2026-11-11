package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type archiveToolBackend struct {
	change  emailtags.ArchiveChange
	failure error
}

func (b *archiveToolBackend) ArchiveMessage(_ context.Context, id int64, change emailtags.ArchiveChange) (*emailtags.ArchiveResult, error) {
	b.change = change
	if b.failure != nil {
		return nil, b.failure
	}
	return &emailtags.ArchiveResult{MessageID: id, Archived: *change.Archived, Changed: true, DryRun: change.DryRun}, nil
}
func TestArchiveMessageToolPolicyAndArguments(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	backend := &archiveToolBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MessageArchive: backend}
	assertionsForTest.NotContains(toolsByName(t, rawListTools(t, opts, false)), ToolArchiveMessage)
	requirementsForTest.Contains(toolsByName(t, rawListTools(t, opts, true)), ToolArchiveMessage)
	result := rawCallTool(t, opts, ToolArchiveMessage, map[string]any{"message_id": float64(7), "archived": false, "dry_run": true})
	requirementsForTest.NotEqual(true, result["isError"])
	requirementsForTest.NotNil(backend.change.Archived)
	assertionsForTest.False(*backend.change.Archived)
	assertionsForTest.True(backend.change.DryRun)
	result = rawCallTool(t, opts, ToolArchiveMessage, map[string]any{"message_id": float64(7)})
	assertionsForTest.Equal(true, result["isError"])
	backend.failure = emailtags.Failure("archive_unsupported", "Gmail only", nil, nil)
	result = rawCallTool(t, opts, ToolArchiveMessage, map[string]any{"message_id": float64(7), "archived": true})
	assertionsForTest.Equal("archive_unsupported", toolStructuredContent(t, result)["error"])
	opts.DelegatedOnly = true
	opts.GrantPermissions = []string{"message.read"}
	assertionsForTest.NotContains(toolsByName(t, rawListTools(t, opts, true)), ToolArchiveMessage)
	opts.GrantPermissions = []string{"message.archive"}
	requirementsForTest.Contains(toolsByName(t, rawListTools(t, opts, true)), ToolArchiveMessage)
	assertionsForTest.NotContains(toolsByName(t, rawListTools(t, opts, false)), ToolArchiveMessage)
}
