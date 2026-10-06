package cmd

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Missing discovery/SDK tools, changed confirmation arguments, implicit native
// writes, discarded receipts, or redispatch on replay break this complete path.
func TestMCPInboxTriageNativeConfirmationAndReceipts(t *testing.T) {
	for _, mode := range []string{"verified", "unknown", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			a, target, writes, lost := nativeInboxTriageFixture(t)
			source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
			var applyCalls atomic.Int64
			// Keep the same real runtime configuration and isolate HTTP credentials.
			a.config.Server.APIKey = "owner-test-key"
			a.config.Server.AgentAccess = true
			daemon := api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/inbox/triage/apply" {
					applyCalls.Add(1)
				}
				daemon.Router().ServeHTTP(w, r)
			}))
			defer server.Close()
			ctx := mcpDraftAgentContext(t, server, source.SourceID, []string{"inbox.read", "inbox.tag"})
			session := mcpDraftTestSession(ctx, t)
			names := mcpDraftToolNames(t, session)
			require.Contains(t, names, "inbox_triage_preview")
			require.Contains(t, names, "inbox_triage_apply")
			arguments := func(value any) map[string]any {
				data, err := json.Marshal(value)
				require.NoError(t, err)
				var args map[string]any
				require.NoError(t, json.Unmarshal(data, &args))
				return args
			}
			input := inboxcontrol.TriageInput{Source: source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}, EvidenceMessageIDs: []int64{target.ItemID}}}}
			if mode == "verified" {
				for _, invalid := range []map[string]any{
					{"source": source, "items": input.Items, "unexpected": true},
					{"source": source, "items": []inboxcontrol.TriageItemInput{{Target: target, EvidenceMessageIDs: []int64{9007199254740992}}}},
					{"source": source, "items": input.Items, "padding": strings.Repeat("x", 1<<20)},
				} {
					result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_preview", Arguments: invalid})
					if callErr == nil {
						require.True(t, result.IsError)
					}
					assert.Zero(t, writes.Load())
					assert.Zero(t, applyCalls.Load())
				}
			}
			preview, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_preview", Arguments: arguments(input)})
			require.NoError(t, err)
			require.False(t, preview.IsError, mcpDraftResultText(t, preview))
			var prepared struct {
				Proposal *inboxcontrol.TriageProposal `json:"proposal"`
			}
			data, err := json.Marshal(preview.StructuredContent)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &prepared))
			require.NotNil(t, prepared.Proposal)
			assert.Zero(t, writes.Load())
			args := arguments(*prepared.Proposal)
			pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args})
			require.NoError(t, err)
			require.True(t, pending.NeedsInput())
			assert.Zero(t, applyCalls.Load())
			assert.Zero(t, writes.Load())
			changed := arguments(*prepared.Proposal)
			changed["preview_token"] = "changed-envelope"
			rejected, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: changed, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
			require.NoError(t, err)
			assert.True(t, rejected.IsError)
			assert.Zero(t, applyCalls.Load())
			// A changed confirmation consumes that interaction; request a fresh one.
			pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args})
			require.NoError(t, err)
			require.True(t, pending.NeedsInput())
			if mode == "unknown" {
				lost.Store(true)
			}
			if mode == "revoked" {
				owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
				require.NoError(t, err)
				defer func() { assert.NoError(t, owner.Close()) }()
				grants, err := owner.ListAgentTokens(t.Context())
				require.NoError(t, err)
				require.Len(t, grants, 1)
				require.NoError(t, owner.RevokeAgentToken(t.Context(), grants[0].ID))
			}
			accepted := &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}}
			result, err := session.CallTool(t.Context(), accepted)
			require.NoError(t, err)
			if mode == "revoked" {
				assert.True(t, result.IsError)
				assert.Zero(t, writes.Load())
				return
			}
			assert.Equal(t, mode == "unknown", result.IsError, mcpDraftResultText(t, result))
			var applied struct {
				Results []inboxcontrol.Result `json:"results"`
				Error   string                `json:"error"`
			}
			data, err = json.Marshal(result.StructuredContent)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &applied))
			require.Len(t, applied.Results, 1)
			require.NotNil(t, applied.Results[0].Receipt)
			want := inboxcontrol.StatusVerified
			if mode == "unknown" {
				want = inboxcontrol.StatusUnknown
				assert.Equal(t, "unknown", applied.Error)
			}
			assert.Equal(t, want, applied.Results[0].Receipt.Status)
			assert.Equal(t, int64(1), writes.Load())
			// Reusing a consumed SDK confirmation cannot dispatch again.
			replay, err := session.CallTool(t.Context(), accepted)
			require.NoError(t, err)
			assert.True(t, replay.IsError)
			assert.Equal(t, int64(1), applyCalls.Load())
			assert.Equal(t, int64(1), writes.Load())
			// An independent confirmed retry recovers the same durable receipt.
			pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args})
			require.NoError(t, err)
			require.True(t, pending.NeedsInput())
			accepted.RequestState = pending.RequestState
			recovered, err := session.CallTool(t.Context(), accepted)
			require.NoError(t, err)
			assert.Equal(t, mode == "unknown", recovered.IsError)
			var recoveredResults struct {
				Results []inboxcontrol.Result `json:"results"`
			}
			data, err = json.Marshal(recovered.StructuredContent)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &recoveredResults))
			require.Len(t, recoveredResults.Results, 1)
			require.NotNil(t, recoveredResults.Results[0].Receipt)
			assert.Equal(t, applied.Results[0].Receipt.ID, recoveredResults.Results[0].Receipt.ID)
			assert.Equal(t, want, recoveredResults.Results[0].Receipt.Status)
			assert.Equal(t, int64(2), applyCalls.Load())
			assert.Equal(t, int64(1), writes.Load())
		})
	}
}

func TestMCPInboxTriageDiscoveryRequiresCurrentReadAndTag(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []string
		admitted    bool
	}{
		{"reader", []string{"inbox.read"}, false},
		{"tag-only", []string{"inbox.tag"}, false},
		{"writer", []string{"inbox.read", "inbox.tag"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, target, writes, _ := nativeInboxTriageFixture(t)
			server := mcpDraftTestDaemon(t, a, nil)
			session := mcpDraftTestSession(mcpDraftAgentContext(t, server, target.SourceID, tc.permissions), t)
			names := mcpDraftToolNames(t, session)
			if tc.admitted {
				assert.Contains(t, names, "inbox_triage_preview")
				assert.Contains(t, names, "inbox_triage_apply")
			} else {
				assert.NotContains(t, names, "inbox_triage_preview")
				assert.NotContains(t, names, "inbox_triage_apply")
			}
			assert.Zero(t, writes.Load())
		})
	}
}
