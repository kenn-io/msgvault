package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestIdentityScoringMCPCatalogRequiresCapabilityAndWriteGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	without := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, true))
	assert.NotContains(without, ToolGetIdentityScoringStatus)
	backend := newIdentityScoringMCPBackend(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityScoring: backend}
	readOnly := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(readOnly, ToolGetIdentityScoringStatus)
	assert.Contains(readOnly, ToolListIdentityJudgments)
	assert.NotContains(readOnly, ToolScoreIdentityMatches)
	writable := toolsByName(t, rawListTools(t, opts, true))
	assert.NotContains(writable, ToolScoreIdentityMatches,
		"generic HTTP write access must not authorize sending identity evidence to Jev")
	consented := ServeOptions{Engine: &querytest.MockEngine{}, IdentityScoring: backend, AllowIdentityScoring: true}
	assert.NotContains(toolsByName(t, rawListTools(t, consented, false)), ToolScoreIdentityMatches,
		"the dedicated opt-in must not bypass the generic HTTP write gate")
	consentedWritable := toolsByName(t, rawListTools(t, consented, true))
	require.Contains(consentedWritable, ToolScoreIdentityMatches)
	annotations, ok := consentedWritable[ToolScoreIdentityMatches]["annotations"].(map[string]any)
	require.True(ok)
	assert.Equal(true, annotations["destructiveHint"],
		"provider disclosure cannot be undone and must be marked destructive")
	assert.Equal(false, annotations["readOnlyHint"])
	assert.Equal(true, annotations["openWorldHint"])
}

func newIdentityScoringMCPBackend(t *testing.T) *daemonclient.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/identity/scoring/status":
			_, _ = io.WriteString(w, `{"enabled":true,"model_id":"jev-1.13.0","minimum_probability":0.8,"batch_size":20,"credential_available":true,"consent_active":true,"dry_run_ready":true,"live_automatic_acceptance_available":false,"live_blocker":"automatic_match_evaluation_required"}`)
		case "/api/v1/identity/scoring/history":
			_, _ = io.WriteString(w, `{"judgments":[],"limit":2,"candidate_id":17}`)
		case "/api/v1/identity/scoring/run":
			_, _ = io.WriteString(w, `{"results":[{"candidate_id":17,"review_token":"token-17","model_id":"jev-1.13.0","packet_schema":"person-match-packet-v1","policy_version":"person-match-policy-v1","evidence_classes":["email"],"probability":0.81,"proposed_action":"needs_review","blockers":["independent_identity_evidence_required"],"status":"scored"}],"processed":1}`)
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	return client
}

func TestIdentityScoringMCPUsesDaemonDryRunAndHistory(t *testing.T) {
	assert := assert.New(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityScoring: newIdentityScoringMCPBackend(t), AllowIdentityScoring: true}
	status := rawCallTool(t, opts, ToolGetIdentityScoringStatus, map[string]any{})
	assert.NotEqual(true, status["isError"])
	assert.Equal(true, toolStructuredContent(t, status)["dry_run_ready"])
	run := confirmedCallTool(t, opts, ToolScoreIdentityMatches, map[string]any{"limit": float64(1)}, true)
	assert.NotEqual(true, run["isError"])
	assert.InDelta(1.0, toolStructuredContent(t, run)["processed"], 0)
	history := rawCallTool(t, opts, ToolListIdentityJudgments, map[string]any{"candidate_id": float64(17), "limit": float64(2), "before_id": float64(99)})
	assert.NotEqual(true, history["isError"])
	assert.InDelta(17.0, toolStructuredContent(t, history)["candidate_id"], 0)
}

func TestIdentityScoringMCPReportsConsentBlockerWithoutGrantingIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/identity/scoring/run", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"consent_required","message":"Consent required"}`)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	result := confirmedCallTool(t, ServeOptions{Engine: &querytest.MockEngine{}, IdentityScoring: client, AllowIdentityScoring: true},
		ToolScoreIdentityMatches, map[string]any{"limit": float64(1)}, true)
	assert.Equal(true, result["isError"])
	content, ok := result["content"].([]any)
	require.True(ok)
	require.Len(content, 1)
	entry, ok := content[0].(map[string]any)
	require.True(ok)
	assert.Contains(entry["text"], "consent_required")
}

func TestIdentityScoringMCPRequiresFreshConfirmationBeforeProviderCall(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var scoreCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/identity/scoring/run", r.URL.Path)
		scoreCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[],"processed":0}`)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityScoring: client, AllowIdentityScoring: true}

	declined := confirmedCallTool(t, opts, ToolScoreIdentityMatches, map[string]any{"limit": float64(1)}, false)
	assert.Equal(true, declined["isError"])
	assert.Equal(int32(0), scoreCalls.Load(), "a declined prompt must not send evidence to the provider")

	accepted := confirmedCallTool(t, opts, ToolScoreIdentityMatches, map[string]any{"limit": float64(1)}, true)
	assert.NotEqual(true, accepted["isError"])
	assert.Equal(int32(1), scoreCalls.Load(), "one fresh confirmation must authorize only its corresponding provider call")
}

func TestIdentityScoringMCPRejectsUnsolicitedConfirmationResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var scoreCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/identity/scoring/run", r.URL.Path)
		scoreCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[],"processed":0}`)
	}))
	t.Cleanup(server.Close)
	backend, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityScoring: backend, AllowIdentityScoring: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(opts, true).Connect(ctx, serverTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "msgvault-confirmation-test", Version: "1.0"}, &sdkmcp.ClientOptions{
		MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true},
	})
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(clientSession.Close()) })

	result, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      ToolScoreIdentityMatches,
		Arguments: map[string]any{"limit": float64(1)},
		InputResponses: sdkmcp.InputResponseMap{
			"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}},
		},
	})
	require.NoError(err)
	assert.True(result.IsError, "a confirmation without a pending challenge must be rejected")
	assert.Equal(int32(0), scoreCalls.Load(), "unsolicited confirmation must not send identity evidence to the provider")

	pending, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      ToolScoreIdentityMatches,
		Arguments: map[string]any{"limit": float64(1)},
	})
	require.NoError(err)
	require.True(pending.NeedsInput(), "an unconfirmed call must create an input challenge")
	require.NotEmpty(pending.RequestState, "the challenge must carry opaque session state")
	confirmed := sdkmcp.InputResponseMap{
		"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}},
	}
	changedArguments, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: ToolScoreIdentityMatches, Arguments: map[string]any{"limit": float64(2)},
		InputResponses: confirmed, RequestState: pending.RequestState,
	})
	require.NoError(err)
	assert.True(changedArguments.IsError, "a challenge must not authorize changed arguments")
	assert.Equal(int32(0), scoreCalls.Load(), "changed arguments must not send identity evidence to the provider")

	replayed, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: ToolScoreIdentityMatches, Arguments: map[string]any{"limit": float64(1)},
		InputResponses: confirmed, RequestState: pending.RequestState,
	})
	require.NoError(err)
	assert.True(replayed.IsError, "a consumed challenge must not be replayable")
	assert.Equal(int32(0), scoreCalls.Load(), "replayed confirmation must not send identity evidence to the provider")
}
