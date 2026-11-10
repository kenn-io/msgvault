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
	return &emailtags.ArchiveResult{MessageID: id, Archived: *change.Archived, Changed: true, DryRun: change.DryRun}, b.failure
}
func TestArchiveMessageToolPolicyAndArguments(t *testing.T) {
	backend := &archiveToolBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MessageArchive: backend}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), ToolArchiveMessage)
	require.Contains(t, toolsByName(t, rawListTools(t, opts, true)), ToolArchiveMessage)
	result := rawCallTool(t, opts, ToolArchiveMessage, map[string]any{"message_id": float64(7), "archived": false, "dry_run": true})
	require.NotEqual(t, true, result["isError"])
	require.NotNil(t, backend.change.Archived)
	assert.False(t, *backend.change.Archived)
	assert.True(t, backend.change.DryRun)
	result = rawCallTool(t, opts, ToolArchiveMessage, map[string]any{"message_id": float64(7)})
	assert.Equal(t, true, result["isError"])
	backend.failure = emailtags.Failure("archive_unsupported", "Gmail only", nil, nil)
	result = rawCallTool(t, opts, ToolArchiveMessage, map[string]any{"message_id": float64(7), "archived": true})
	assert.Equal(t, "archive_unsupported", toolStructuredContent(t, result)["error"])
	opts.DelegatedOnly = true
	opts.GrantPermissions = []string{"message.read"}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), ToolArchiveMessage)
	opts.GrantPermissions = []string{"message.archive"}
	require.Contains(t, toolsByName(t, rawListTools(t, opts, true)), ToolArchiveMessage)
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), ToolArchiveMessage)
}
