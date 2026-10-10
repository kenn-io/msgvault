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
			assertions := assert.New(t)
			requirements := require.New(t)

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
			requirements.Contains(names, "inbox_triage_preview")
			requirements.Contains(names, "inbox_triage_apply")
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
						requirements.True(result.IsError)
					}
					assertions.Zero(writes.Load())
					assertions.Zero(applyCalls.Load())
				}
			}
			preview, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_preview", Arguments: arguments(input)})
			requirements.NoError(err)
			requirements.False(preview.IsError, mcpDraftResultText(t, preview))
			var prepared struct {
				Proposal *inboxcontrol.TriageProposal `json:"proposal"`
			}
			data, err := json.Marshal(preview.StructuredContent)
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal(data, &prepared))
			requirements.NotNil(prepared.Proposal)
			assertions.Zero(writes.Load())
			args := arguments(*prepared.Proposal)
			pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args})
			requirements.NoError(err)
			requirements.True(pending.NeedsInput())
			assertions.Zero(applyCalls.Load())
			assertions.Zero(writes.Load())
			changed := arguments(*prepared.Proposal)
			changed["preview_token"] = "changed-envelope"
			rejected, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: changed, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
			requirements.NoError(err)
			assertions.True(rejected.IsError)
			assertions.Zero(applyCalls.Load())
			// A changed confirmation consumes that interaction; request a fresh one.
			pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args})
			requirements.NoError(err)
			requirements.True(pending.NeedsInput())
			if mode == "unknown" {
				lost.Store(true)
			}
			if mode == "revoked" {
				owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
				requirements.NoError(err)
				defer func() { assert.NoError(t, owner.Close()) }()
				grants, err := owner.ListAgentTokens(t.Context())
				requirements.NoError(err)
				requirements.Len(grants, 1)
				requirements.NoError(owner.RevokeAgentToken(t.Context(), grants[0].ID))
			}
			accepted := &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}}
			result, err := session.CallTool(t.Context(), accepted)
			requirements.NoError(err)
			if mode == "revoked" {
				assertions.True(result.IsError)
				assertions.Zero(writes.Load())
				return
			}
			assertions.Equal(mode == "unknown", result.IsError, mcpDraftResultText(t, result))
			var applied struct {
				Results []inboxcontrol.Result `json:"results"`
				Error   string                `json:"error"`
			}
			data, err = json.Marshal(result.StructuredContent)
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal(data, &applied))
			requirements.Len(applied.Results, 1)
			requirements.NotNil(applied.Results[0].Receipt)
			want := inboxcontrol.StatusVerified
			if mode == "unknown" {
				want = inboxcontrol.StatusUnknown
				assertions.Equal("unknown", applied.Error)
			}
			assertions.Equal(want, applied.Results[0].Receipt.Status)
			assertions.Equal(int64(1), writes.Load())
			// Reusing a consumed SDK confirmation cannot dispatch again.
			replay, err := session.CallTool(t.Context(), accepted)
			requirements.NoError(err)
			assertions.True(replay.IsError)
			assertions.Equal(int64(1), applyCalls.Load())
			assertions.Equal(int64(1), writes.Load())
			// An independent confirmed retry recovers the same durable receipt.
			pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_triage_apply", Arguments: args})
			requirements.NoError(err)
			requirements.True(pending.NeedsInput())
			accepted.RequestState = pending.RequestState
			recovered, err := session.CallTool(t.Context(), accepted)
			requirements.NoError(err)
			assertions.Equal(mode == "unknown", recovered.IsError)
			var recoveredResults struct {
				Results []inboxcontrol.Result `json:"results"`
			}
			data, err = json.Marshal(recovered.StructuredContent)
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal(data, &recoveredResults))
			requirements.Len(recoveredResults.Results, 1)
			requirements.NotNil(recoveredResults.Results[0].Receipt)
			assertions.Equal(applied.Results[0].Receipt.ID, recoveredResults.Results[0].Receipt.ID)
			assertions.Equal(want, recoveredResults.Results[0].Receipt.Status)
			assertions.Equal(int64(2), applyCalls.Load())
			assertions.Equal(int64(1), writes.Load())
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
			assertions := assert.New(t)

			a, target, writes, _ := nativeInboxTriageFixture(t)
			server := mcpDraftTestDaemon(t, a, nil)
			session := mcpDraftTestSession(mcpDraftAgentContext(t, server, target.SourceID, tc.permissions), t)
			names := mcpDraftToolNames(t, session)
			if tc.admitted {
				assertions.Contains(names, "inbox_triage_preview")
				assertions.Contains(names, "inbox_triage_apply")
			} else {
				assertions.NotContains(names, "inbox_triage_preview")
				assertions.NotContains(names, "inbox_triage_apply")
			}
			assertions.Zero(writes.Load())
		})
	}
}
