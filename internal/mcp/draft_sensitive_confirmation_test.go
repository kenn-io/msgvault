package mcp

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sensitiveDraftSession(t *testing.T, opts ServeOptions) *sdkmcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(opts, true).Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "sensitive-draft-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestSensitiveDraftConfirmation(t *testing.T) {
	for _, tc := range []struct {
		command, tool string
		args          map[string]any
	}{
		{"draft-compose", ToolDraftCompose, map[string]any{"to": []string{"recipient@example.com"}, "body": "password: synthetic-password"}},
		{"draft-reply", ToolDraftReply, map[string]any{"message_id": 7, "body": "password: synthetic-password"}},
		{"draft-forward", ToolDraftForward, map[string]any{"message_id": 7, "body": "password: synthetic-password"}},
		{"draft-edit", ToolDraftEdit, map[string]any{"draft_id": "d1", "revision": 2, "body": "password: synthetic-password"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
			session := sensitiveDraftSession(t, ServeOptions{Drafts: runner, DraftCommands: []string{tc.command}, ConfirmSensitiveDrafts: true})
			for _, response := range []*sdkmcp.ElicitResult{{Action: "decline"}, {Action: "cancel"}, {Action: "accept", Content: map[string]any{"confirm": false}}} {
				pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
				require.NoError(err)
				require.True(pending.NeedsInput())
				assert.Zero(runner.calls)
				prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
				require.True(ok)
				assert.Contains(prompt.Message, tc.tool)
				assert.Contains(prompt.Message, "password")
				assert.NotContains(prompt.Message, "synthetic-password")
				encoded, err := json.Marshal(pending)
				require.NoError(err)
				assert.NotContains(string(encoded), "synthetic-password")
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.tool, Arguments: tc.args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": response}})
				require.NoError(err)
				assert.True(result.IsError)
				assert.Zero(runner.calls)
			}
			pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			require.NoError(err)
			require.True(pending.NeedsInput())
			call := &sdkmcp.CallToolParams{Name: tc.tool, Arguments: tc.args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}}
			result, err := session.CallTool(t.Context(), call)
			require.NoError(err)
			assert.False(result.IsError)
			assert.False(result.NeedsInput())
			assert.Equal(1, runner.calls)
			assert.Equal([]string{"password: synthetic-password"}, runner.request.Flags["body"])
			var receipt map[string]any
			encoded, err := json.Marshal(result.StructuredContent)
			require.NoError(err)
			require.NoError(json.Unmarshal(encoded, &receipt))
			assert.Contains(receipt, "warnings")
			result, err = session.CallTool(t.Context(), call)
			require.NoError(err)
			assert.True(result.IsError)
			assert.Equal(1, runner.calls)
		})
	}
}

func TestSensitiveDraftConfirmationDefaultAndBenignParity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		enabled    bool
	}{
		{"default off", "password: synthetic-password", false},
		{"benign", "Please send the report.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
			session := sensitiveDraftSession(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: tc.enabled})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: map[string]any{"body": tc.body}})
			require.NoError(t, err)
			assert.False(result.IsError)
			assert.False(result.NeedsInput())
			assert.Equal(1, runner.calls)
		})
	}
}

func TestSensitiveDraftConfirmationDoesNotTrustQuoteMarkers(t *testing.T) {
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
	session := sensitiveDraftSession(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: true})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: map[string]any{"body": "> password: synthetic-password"}})
	require.NoError(t, err)
	assert.True(t, result.NeedsInput())
	assert.Zero(t, runner.calls)
}

func TestSensitiveDraftSubjectConfirmation(t *testing.T) {
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
	session := sensitiveDraftSession(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: true})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: map[string]any{"subject": "password: synthetic-password", "body": "Please see the subject."}})
	require.NoError(t, err)
	assert.True(t, result.NeedsInput())
	assert.Zero(t, runner.calls)
}

func TestSensitiveDraftConfirmationBindsExactArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"benign replacement", map[string]any{"body": "Please send the report."}},
		{"changed secret", map[string]any{"body": "password: different-synthetic-password"}},
		{"changed recipient", map[string]any{"body": "password: synthetic-password", "to": []string{"other@example.com"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
			session := sensitiveDraftSession(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: true})
			pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: map[string]any{"body": "password: synthetic-password"}})
			require.NoError(err)
			require.True(pending.NeedsInput())
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: tc.args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
			require.NoError(err)
			assert.True(result.IsError)
			assert.Zero(runner.calls)
		})
	}
}

func TestSensitiveDraftConfirmationHTTPBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
	opts := ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: true}
	issuer := newMCPHTTPServer(opts, HTTPOptions{APIKey: "test-key", AllowWrites: true}).Handler
	other := newMCPHTTPServer(opts, HTTPOptions{APIKey: "test-key", AllowWrites: true}).Handler
	call := func(handler http.Handler, state any) task3RPCResponse {
		t.Helper()
		params := map[string]any{"name": ToolDraftCompose, "arguments": map[string]any{"body": "password: synthetic-password"}, "_meta": modernRequestMeta()}
		if state != nil {
			params["requestState"] = state
			params["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		require.NoError(err)
		request := task3ModernRequest("tools/call", ToolDraftCompose, string(body))
		request.Header.Set("Authorization", "Bearer test-key")
		recorder, response := task3Serve(handler, request)
		task3RequireSuccess(t, recorder, response)
		assert.NotContains(recorder.Body.String(), "synthetic-password")
		return response
	}
	pending := call(issuer, nil)
	require.NotEmpty(pending.Result["inputRequests"])
	state := pending.Result["requestState"]
	require.NotEmpty(state)
	assert.Zero(runner.calls)
	refused := call(other, state)
	assert.Equal(true, refused.Result["isError"])
	assert.Zero(runner.calls)
	accepted := call(issuer, state)
	assert.NotEqual(true, accepted.Result["isError"])
	assert.Equal(1, runner.calls)
	replay := call(issuer, state)
	assert.Equal(true, replay.Result["isError"])
	assert.Equal(1, runner.calls)
	assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), ToolDraftCompose)
}

func TestSensitiveDraftLegacyClientWithoutElicitation(t *testing.T) {
	assert := assert.New(t)
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
	peer := newTask5RawStdioPeer(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: true})
	initialized := peer.call(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"sensitive-legacy-test","version":"1"}}}`)
	require.Nil(t, initialized.Error)
	response := peer.call(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"draft_compose","arguments":{"body":"password: synthetic-password"}}}`)
	assert.Equal(true, response.Result["isError"])
	assert.Contains(task3ToolErrorText(t, response), "client confirmation is unavailable")
	assert.NotContains(task3ToolErrorText(t, response), "synthetic-password")
	assert.Zero(runner.calls)
}

func TestSensitiveDraftNumericFieldsAndTokenContext(t *testing.T) {
	token := strings.Join([]string{"aB1", "cD2", "eF3", "gH4", "iJ5", "kL6", "mN7", "oP8", "qR9"}, "_")
	for _, body := range []string{
		"4111 1111 1111 1111 12/27",
		"4111111111111111 5555555555554444",
		"Call /api with token: " + token,
	} {
		t.Run(body, func(t *testing.T) {
			runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
			session := sensitiveDraftSession(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: true})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: map[string]any{"body": body}})
			require.NoError(t, err)
			assert.True(t, result.NeedsInput())
			assert.Zero(t, runner.calls)
		})
	}
}

func FuzzSensitiveDraftConfirmationRejectsChangedBody(f *testing.F) {
	f.Add("Please send the report.")
	f.Add("password: different-synthetic-password")
	f.Fuzz(func(t *testing.T, body string) {
		if body == "password: synthetic-password" || !utf8.ValidString(body) {
			t.Skip()
		}
		assert := assert.New(t)
		require := require.New(t)
		runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created"}`}}
		session := sensitiveDraftSession(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-compose"}, ConfirmSensitiveDrafts: true})
		pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: map[string]any{"body": "password: synthetic-password"}})
		require.NoError(err)
		require.True(pending.NeedsInput())
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftCompose, Arguments: map[string]any{"body": body}, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
		require.NoError(err)
		assert.True(result.IsError)
		assert.Zero(runner.calls)
	})
}
