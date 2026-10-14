package mcp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmationChallengesRejectChangedDisclosureAndExpiry(t *testing.T) {
	cases := []struct {
		name, message string
		expired       bool
	}{
		{"changed disclosure", "Sync source 2", false},
		{"expired", "Sync source 1", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			manager := newConfirmationChallenges()
			now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
			manager.now = func() time.Time { return now }
			state, err := manager.issue(nil, "client-a", "sync_source", map[string]any{"source_id": float64(1)}, "Sync source 1")
			requirements.NoError(err)
			if tt.expired {
				now = now.Add(confirmationChallengeTTL)
			}
			assertions.False(manager.consume(state, nil, "client-a", "sync_source", map[string]any{"source_id": float64(1)}, tt.message))
			assertions.False(manager.consume(state, nil, "client-a", "sync_source", map[string]any{"source_id": float64(1)}, "Sync source 1"))
		})
	}
}

func TestConfirmationChallengesBoundAndPurge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	manager := newConfirmationChallenges()
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	for range maxPendingConfirmations {
		_, err := manager.issue(nil, "client-a", "sync_source", nil, "Sync")
		requirements.NoError(err)
	}
	_, err := manager.issue(nil, "client-a", "sync_source", nil, "Sync")
	requirements.Error(err)
	now = now.Add(confirmationChallengeTTL)
	_, err = manager.issue(nil, "client-a", "sync_source", nil, "Sync")
	requirements.NoError(err)
	assertions.Len(manager.pending, 1)
}

func TestConfirmationModernStdioSDKObtainsApproval(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "confirmation-test", Version: "1"}, nil)
	var approved, executed atomic.Int32
	sdkmcp.AddTool[map[string]any, any](server, &sdkmcp.Tool{Name: "synthetic_write", InputSchema: closedObject(nil), OutputSchema: closedObject(nil)}, officialToolHandler(func(ctx context.Context, req toolRequest) (*toolResult, error) {
		if err := req.confirmUserAction(ctx, "Perform synthetic action"); err != nil {
			return confirmationToolError(err)
		}
		executed.Add(1)
		return jsonResult(map[string]any{})
	}))
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	requirements.NoError(err)
	defer func() { _ = serverSession.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "client", Version: "1"}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Equal("Perform synthetic action", request.Params.Message)
		approved.Add(1)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	session, err := client.Connect(ctx, clientTransport, nil)
	requirements.NoError(err)
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "synthetic_write"})
	requirements.NoError(err)
	assertions.False(result.IsError)
	assertions.Equal(int32(1), approved.Load())
	assertions.Equal(int32(1), executed.Load())
}

func TestConfirmationChallengeCrossesStatelessRequestsWithinAuthenticatedSession(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	confirmations := newConfirmationChallenges()
	arguments := map[string]any{"candidate_id": float64(7)}
	const sessionKey = "authenticated-session"
	token, err := confirmations.issue(nil, sessionKey, ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)

	asserts.True(confirmations.consume(token, nil, sessionKey, ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, sessionKey, ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"),
		"confirmation challenges must be one-time")
}

func TestConfirmationChallengeCannotCrossAuthenticatedSessionsOrActions(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	confirmations := newConfirmationChallenges()
	arguments := map[string]any{"candidate_id": float64(7)}
	token, err := confirmations.issue(nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)

	asserts.False(confirmations.consume(token, nil, "session-b", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"),
		"an attempted use from another session must consume the one-time challenge")
	token, err = confirmations.issue(nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)
	asserts.False(confirmations.consume(token, nil, "session-a", ToolRejectIdentityMatch, arguments, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
	token, err = confirmations.issue(nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch,
		map[string]any{"candidate_id": float64(8)}, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
}
