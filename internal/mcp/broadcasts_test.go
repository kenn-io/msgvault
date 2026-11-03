package mcp

import (
	"context"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
)

type broadcastTestBackend struct{ detail api.BroadcastDetail }

func (b *broadcastTestBackend) GetBroadcast(context.Context, string) (api.BroadcastDetail, error) {
	return b.detail, nil
}
func (b *broadcastTestBackend) ListBroadcastSubscriptions(context.Context, string, int64, string) (api.BroadcastSubscriptionsResponse, error) {
	return api.BroadcastSubscriptionsResponse{}, nil
}
func broadcastTestOptions(runner *draftTestRunner) ServeOptions {
	return ServeOptions{Drafts: runner, BroadcastAPISchemaVersion: BroadcastMinAPISchemaVersion, Broadcasts: &broadcastTestBackend{detail: api.BroadcastDetail{BroadcastSummary: api.BroadcastSummary{ID: "broadcast-abc", Revision: 1, PlanDigest: "digest", DailyBudgetCents: 500, CampaignDays: 7, PageID: "100"}, Recipients: []api.BroadcastRecipientView{{SubscriptionID: 1, State: "planned"}, {SubscriptionID: 2, State: "refused"}}}}}
}
func broadcastTestSession(t *testing.T, opts ServeOptions) *sdkmcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(opts, true).Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "broadcast-fixture", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}
func TestMCPSendBroadcastRequiresDigest(t *testing.T) {
	assertions := assert.New(t)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"broadcast_id": "broadcast-abc", "revision": float64(1)}, "plan_digest is required"},
		{map[string]any{"broadcast_id": "broadcast-abc", "plan_digest": "digest"}, "revision parameter is required"},
	} {
		runner := &draftTestRunner{}
		h := &handlers{drafts: runner, broadcasts: broadcastTestOptions(runner).Broadcasts}
		result, err := broadcastToolHandler("send")(h, t.Context(), toolRequest{arguments: tc.args})
		require.NoError(t, err)
		assertions.True(result.isError)
		assertions.Equal(tc.want, result.text)
		assertions.Zero(runner.calls)
	}
}
func TestMCPSendBroadcastDeclinedConfirmationNoRun(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"state":"paused"}`}}
	session := broadcastTestSession(t, broadcastTestOptions(runner))
	args := map[string]any{"broadcast_id": "broadcast-abc", "revision": float64(1), "plan_digest": "digest"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSendBroadcast, Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	assertions.Zero(runner.calls)
	prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	requirements.True(ok)
	for _, value := range []string{"1 recipient", "500", "875", "3500", "7 days", "100"} {
		assertions.Contains(prompt.Message, value)
	}
	declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSendBroadcast, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "decline"}}})
	requirements.NoError(err)
	assertions.True(declined.IsError)
	assertions.Zero(runner.calls)
	pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSendBroadcast, Arguments: args})
	requirements.NoError(err)
	accepted, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSendBroadcast, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	assertions.False(accepted.IsError)
	assertions.Equal(1, runner.calls)
	assertions.Equal("broadcast-send", runner.request.Command)
}
func TestMCPSendBroadcastNoElicitationFailsClosed(t *testing.T) {
	runner := &draftTestRunner{}
	h := &handlers{drafts: runner, broadcasts: broadcastTestOptions(runner).Broadcasts}
	result, err := broadcastToolHandler("send")(h, t.Context(), toolRequest{arguments: map[string]any{"broadcast_id": "broadcast-abc", "revision": float64(1), "plan_digest": "digest"}})
	require.NoError(t, err)
	assert.True(t, result.isError)
	assert.Zero(t, runner.calls)
}
func TestMCPBroadcastToolsHiddenDelegatedOnly(t *testing.T) {
	opts := broadcastTestOptions(&draftTestRunner{})
	opts.DelegatedOnly = true
	for _, d := range operationCatalog(opts, nil) {
		assert.NotEqual(t, ToolSendBroadcast, d.name)
		assert.NotEqual(t, ToolGetBroadcast, d.name)
	}
}
func TestMCPBroadcastToolsGatedBySchema(t *testing.T) {
	for _, version := range []string{"", "3.29.0", BroadcastMinAPISchemaVersion} {
		opts := broadcastTestOptions(&draftTestRunner{})
		opts.BroadcastAPISchemaVersion = version
		found := false
		for _, d := range operationCatalog(opts, nil) {
			found = found || d.name == ToolSendBroadcast
		}
		assert.Equal(t, version == BroadcastMinAPISchemaVersion, found)
	}
}
func TestMCPSendBroadcastChangedPlanRefusedBeforeConfirmation(t *testing.T) {
	assertions := assert.New(t)
	runner := &draftTestRunner{}
	opts := broadcastTestOptions(runner)
	backend, ok := opts.Broadcasts.(*broadcastTestBackend)
	require.True(t, ok)
	backend.detail.CampaignEndsAt = new(time.Now().Add(time.Hour))
	h := &handlers{drafts: runner, broadcasts: backend}
	result, err := broadcastToolHandler("send")(h, t.Context(), toolRequest{arguments: map[string]any{"broadcast_id": "broadcast-abc", "revision": float64(2), "plan_digest": "digest"}})
	require.NoError(t, err)
	assertions.True(result.isError)
	assertions.Equal("plan_changed", result.text)
	assertions.Zero(runner.calls)
}
