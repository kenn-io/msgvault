package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type tagsToolBackend struct {
	change *emailtags.MessageTagChange
	fail   bool
	calls  int
}

func (b *tagsToolBackend) MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string) (*emailtags.MessageTagResult, error) {
	b.calls++
	b.change = change
	result := &emailtags.MessageTagResult{MessageID: id, SourceID: 2, Provider: "imap", Tags: []string{"Next"}, Verified: true}
	if b.fail {
		return result, emailtags.Failure("remote_unknown", "Read tags before retrying", result, nil)
	}
	return result, nil
}

func TestEmailTagsMaskedOwnerRequiresExplicitUnmask(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend := &tagsToolBackend{fail: true}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MessageTags: backend}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: ToolGetMessageTags, args: map[string]any{"message_id": float64(7)}},
		{name: ToolUpdateMessageTags, args: map[string]any{"message_id": float64(7), "add": []any{"Next"}}},
	} {
		response := rawModernCall(t, opts, HTTPOptions{AllowWrites: true}, "tools/call", map[string]any{
			"name": tc.name, "arguments": tc.args,
		})
		got := response.Result
		assertions.Equal(true, got["isError"], tc.name)
		assertions.Contains(fmt.Sprint(got), "verification_masked", tc.name)
	}
	assertions.Zero(backend.calls, "masked success and error results must not reach the backend")

	opts.UnmaskVerification = true
	for _, name := range []string{ToolGetMessageTags, ToolUpdateMessageTags} {
		args := map[string]any{"message_id": float64(7)}
		if name == ToolUpdateMessageTags {
			args["add"] = []any{"Next"}
		}
		response := rawModernCall(t, opts, HTTPOptions{AllowWrites: true}, "tools/call", map[string]any{
			"name": name, "arguments": args,
		})
		got := response.Result
		assertions.Equal(true, got["isError"], name, "unmasked calls should reach the failing backend")
	}
	requirements.Equal(2, backend.calls)
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
