package mcp

import (
	"context"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
	assertions := assert.New(t)
	requirements := require.New(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}}
	absent := toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(absent, ToolGetMessageTags)
	backend := &tagsToolBackend{}
	opts.MessageTags = backend
	read := toolsByName(t, rawListTools(t, opts, false))
	requirements.Contains(read, ToolGetMessageTags)
	assertions.NotContains(read, ToolUpdateMessageTags)
	write := toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(write, ToolUpdateMessageTags)
	assertions.Equal(false, toolReadOnlyHint(t, write[ToolUpdateMessageTags]))
	got := rawCallTool(t, opts, ToolUpdateMessageTags, map[string]any{"message_id": float64(7), "add": []any{"Next"}, "dry_run": true})
	requirements.NotEqual(true, got["isError"])
	requirements.NotNil(backend.change)
	assertions.True(backend.change.DryRun)
	backend.fail = true
	got = rawCallTool(t, opts, ToolUpdateMessageTags, map[string]any{"message_id": float64(7), "add": []any{"Next"}, "dry_run": true})
	assertions.Equal(true, got["isError"])
	data := toolStructuredContent(t, got)
	assertions.Equal("remote_unknown", data["error"])
	requirements.Contains(data, "result")
}

func TestEmailTagsReadOnlyBackendSuppressesWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	backend := &tagsToolBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MessageTags: backend, SuppressMessageTagWrites: true}
	names := toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(names, ToolGetMessageTags)
	assertions.NotContains(names, ToolUpdateMessageTags)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(opts, true).Connect(t.Context(), serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "msgvault-tag-admission-test", Version: "1.0"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: map[string]any{"message_id": float64(7), "add": []any{"Next"}}})
	requirements.Error(err)
	assertions.Contains(err.Error(), "unknown tool")
	assertions.Nil(backend.change)
}

func TestEmailTagsMutationRequiresClientConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		response  *sdkmcp.ElicitResult
		dryRun    bool
		wantWrite bool
	}{
		{name: "unsupported"},
		{name: "declined", response: &sdkmcp.ElicitResult{Action: "decline"}},
		{name: "cancelled", response: &sdkmcp.ElicitResult{Action: "cancel"}},
		{name: "accept without approval", response: &sdkmcp.ElicitResult{Action: "accept"}},
		{name: "approved", response: &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, wantWrite: true},
		{name: "preview", dryRun: true, wantWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			backend := &tagsToolBackend{}
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, MessageTags: backend}, true).Connect(t.Context(), serverTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assertions.NoError(server.Close()) })
			options := &sdkmcp.ClientOptions{}
			confirmations := 0
			if tc.response != nil {
				options.ElicitationHandler = func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
					confirmations++
					assertions.Contains(request.Params.Message, `"message_id":7`)
					assertions.Contains(request.Params.Message, `"mailbox":"INBOX"`)
					assertions.Contains(request.Params.Message, `"add":["Next"]`)
					return tc.response, nil
				}
			}
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "tag-confirmation-test", Version: "1"}, options)
			session, err := client.Connect(t.Context(), clientTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assertions.NoError(session.Close()) })
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: map[string]any{"message_id": 7, "mailbox": "INBOX", "add": []string{"Next"}, "dry_run": tc.dryRun}})
			if tc.wantWrite {
				requirements.NoError(err)
				requirements.NotNil(result)
				assertions.False(result.IsError)
				requirements.NotNil(backend.change)
				assertions.Equal(tc.dryRun, backend.change.DryRun)
			} else {
				assertions.True(err != nil || (result != nil && result.IsError))
				assertions.Nil(backend.change, "unapproved calls must stop before provider dispatch")
			}
			if tc.response != nil {
				assertions.Equal(1, confirmations)
			}
		})
	}
}
