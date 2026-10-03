package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type tagsToolBackend struct {
	change *emailtags.Change
	fail   bool
}

func (b *tagsToolBackend) MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error) {
	b.change = change
	result := &emailtags.Result{MessageID: id, SourceID: 2, Provider: "imap", Tags: []string{"Next"}, Verified: true}
	if b.fail {
		return result, emailtags.Failure("remote_unknown", "Read tags before retrying", result, nil)
	}
	return result, nil
}
func TestEmailTagsToolsAvailabilityAndPartialResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}}
	absent := toolsByName(t, rawListTools(t, opts, true))
	assert.NotContains(absent, ToolGetMessageTags)
	backend := &tagsToolBackend{}
	opts.MessageTags = backend
	read := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(read, ToolGetMessageTags)
	assert.NotContains(read, ToolUpdateMessageTags)
	write := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(write, ToolUpdateMessageTags)
	assert.Equal(false, toolReadOnlyHint(t, write[ToolUpdateMessageTags]))
	got := rawCallTool(t, opts, ToolUpdateMessageTags, map[string]any{"message_id": float64(7), "add": []any{"Next"}, "dry_run": true})
	require.NotEqual(true, got["isError"])
	require.NotNil(backend.change)
	assert.True(backend.change.DryRun)
	backend.fail = true
	got = rawCallTool(t, opts, ToolUpdateMessageTags, map[string]any{"message_id": float64(7), "add": []any{"Next"}})
	assert.Equal(true, got["isError"])
	data := toolStructuredContent(t, got)
	assert.Equal("remote_unknown", data["error"])
	require.Contains(data, "result")
}
