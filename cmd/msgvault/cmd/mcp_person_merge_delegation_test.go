package cmd

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPDelegatedPersonMergeProductionDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name               string
		permissions        []string
		optIn, read, write bool
	}{
		{"reader", []string{"person.read"}, true, true, false},
		{"merger", []string{"person.read", "person.merge"}, true, true, true},
		{"writes disabled", []string{"person.read", "person.merge"}, false, true, false},
		{"missing read", []string{"person.merge"}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			previous := mcpAllowPersonMerges
			mcpAllowPersonMerges = tc.optIn
			t.Cleanup(func() { mcpAllowPersonMerges = previous })
			fixture := newDraftReplyFixture(t)
			participant, err := fixture.store.EnsureParticipant("mcp-merge-survivor@example.test", "MCP Survivor", "example.test")
			requirements.NoError(err)
			survivor, _, err := fixture.store.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			participant, err = fixture.store.EnsureParticipant("mcp-merge-absorbed@example.test", "MCP Absorbed", "example.test")
			requirements.NoError(err)
			absorbed, _, err := fixture.store.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			var requestNumber atomic.Uint64
			server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.RemoteAddr = fmt.Sprintf("192.0.2.%d:12345", requestNumber.Add(1))
					next.ServeHTTP(w, r)
				})
			})
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = owner.Close() })
			response, err := owner.DoGeneratedRequestWithContext(t.Context(), http.MethodPost, "/api/v1/agent-tokens", &generated.IssueAgentTokenRequestOptions{Body: &generated.IssueAgentTokenBody{Label: "Synthetic merge MCP", Permissions: tc.permissions, PersonIds: []int64{survivor.ID, absorbed.ID}}})
			requirements.NoError(err)
			defer func() { assertions.NoError(response.Body.Close()) }()
			requirements.Equal(http.StatusCreated, response.StatusCode)
			var grant generated.AgentTokenIssueResponse
			requirements.NoError(json.UnmarshalRead(response.Body, &grant))
			tokenFile := filepath.Join(t.TempDir(), "agent.token")
			requirements.NoError(os.WriteFile(tokenFile, []byte(grant.Secret+"\n"), 0o600))
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = t.TempDir()
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
			session := mcpDraftTestSession(ctx, t)
			names := mcpDraftToolNames(t, session)
			for _, entry := range []struct {
				name string
				want bool
			}{{mcpserver.ToolGetPersonMergeContext, tc.read}, {mcpserver.ToolMergePerson, tc.write}} {
				if entry.want {
					assertions.Contains(names, entry.name)
				} else {
					assertions.NotContains(names, entry.name)
				}
			}
			for _, name := range []string{mcpserver.ToolGetCardDAVPublication, mcpserver.ToolPreviewCardDAVPublication, mcpserver.ToolApproveCardDAVPublication, mcpserver.ToolSyncCardDAV, mcpserver.ToolGetCardDAVSyncStatus, "search_metadata", "promote_person"} {
				assertions.NotContains(names, name)
			}
			if tc.read {
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolGetPersonMergeContext, Arguments: map[string]any{"survivor_person_id": survivor.ID, "absorbed_person_id": absorbed.ID}})
				requirements.NoError(err)
				requirements.False(result.IsError, mcpDraftResultText(t, result))
				var context daemonclient.PersonMergeContext
				encoded, err := json.Marshal(result.StructuredContent)
				requirements.NoError(err)
				requirements.NoError(json.Unmarshal(encoded, &context))
				assertions.Equal(survivor.ID, context.Survivor.ID)
				assertions.Equal(absorbed.ID, context.Absorbed.ID)
				assertions.NotEmpty(context.SurvivorETag)
				assertions.NotEmpty(context.AbsorbedETag)
			}
			current, err := fixture.store.GetPerson(survivor.ID)
			requirements.NoError(err)
			assertions.Equal(survivor, current)
			current, err = fixture.store.GetPerson(absorbed.ID)
			requirements.NoError(err)
			assertions.Equal(absorbed, current)
		})
	}
}

func TestMCPDelegatedPersonMergeProductionConfirmation(t *testing.T) {
	for _, name := range []string{"commit and replay", "cancel", "revoke before confirmation"} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			previous := mcpAllowPersonMerges
			mcpAllowPersonMerges = true
			t.Cleanup(func() { mcpAllowPersonMerges = previous })
			fixture := newDraftReplyFixture(t)
			participant, err := fixture.store.EnsureParticipant("confirmed-survivor@example.test", "Confirmed Survivor", "example.test")
			requirements.NoError(err)
			survivor, _, err := fixture.store.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			participant, err = fixture.store.EnsureParticipant("confirmed-absorbed@example.test", "Confirmed Absorbed", "example.test")
			requirements.NoError(err)
			absorbed, _, err := fixture.store.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			// Nameless profiles keep the confirmation text stable after revocation,
			// so the second call reaches the native endpoint instead of a changed-prompt guard.
			survivor, err = fixture.store.UpdatePersonDisplayNameContext(t.Context(), survivor.ID, survivor.Revision, nil)
			requirements.NoError(err)
			absorbed, err = fixture.store.UpdatePersonDisplayNameContext(t.Context(), absorbed.ID, absorbed.Revision, nil)
			requirements.NoError(err)
			var requestNumber atomic.Uint64
			var mergePosts atomic.Uint64
			server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.RemoteAddr = fmt.Sprintf("192.0.2.%d:12345", requestNumber.Add(1))
					if r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/api/v1/people/%d/merge", survivor.ID) {
						mergePosts.Add(1)
					}
					next.ServeHTTP(w, r)
				})
			})
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = owner.Close() })
			response, err := owner.DoGeneratedRequestWithContext(t.Context(), http.MethodPost, "/api/v1/agent-tokens", &generated.IssueAgentTokenRequestOptions{Body: &generated.IssueAgentTokenBody{Label: "Synthetic confirmation merge", Permissions: []string{"person.read", "person.merge"}, PersonIds: []int64{survivor.ID, absorbed.ID}}})
			requirements.NoError(err)
			requirements.Equal(http.StatusCreated, response.StatusCode)
			var grant generated.AgentTokenIssueResponse
			requirements.NoError(json.UnmarshalRead(response.Body, &grant))
			requirements.NoError(response.Body.Close())
			tokenFile := filepath.Join(t.TempDir(), "agent.token")
			requirements.NoError(os.WriteFile(tokenFile, []byte(grant.Secret+"\n"), 0o600))
			cfg := config.NewDefaultConfig()
			cfg.HomeDir, cfg.Data.DataDir = t.TempDir(), t.TempDir()
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
			session := mcpDraftTestSession(ctx, t)
			contextResult, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolGetPersonMergeContext, Arguments: map[string]any{"survivor_person_id": survivor.ID, "absorbed_person_id": absorbed.ID}})
			requirements.NoError(err)
			requirements.False(contextResult.IsError, mcpDraftResultText(t, contextResult))
			var mergeContext daemonclient.PersonMergeContext
			encoded, err := json.Marshal(contextResult.StructuredContent)
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal(encoded, &mergeContext))
			args := map[string]any{"survivor_person_id": survivor.ID, "absorbed_person_id": absorbed.ID, "survivor_etag": mergeContext.SurvivorETag, "absorbed_etag": mergeContext.AbsorbedETag, "idempotency_key": "synthetic-sdk-confirmed-merge"}
			pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolMergePerson, Arguments: args})
			requirements.NoError(err)
			requirements.True(pending.NeedsInput())
			assertions.Equal(uint64(0), mergePosts.Load())
			current, err := fixture.store.GetPerson(survivor.ID)
			requirements.NoError(err)
			assertions.Equal(survivor, current)
			current, err = fixture.store.GetPerson(absorbed.ID)
			requirements.NoError(err)
			assertions.Equal(absorbed, current)
			action := "accept"
			if name == "cancel" {
				action = "decline"
			}
			if name == "revoke before confirmation" {
				revoked, revokeErr := owner.DoGeneratedRequestWithContext(t.Context(), http.MethodDelete, "/api/v1/agent-tokens/"+grant.ID, nil)
				requirements.NoError(revokeErr)
				assertions.Equal(http.StatusNoContent, revoked.StatusCode)
				requirements.NoError(revoked.Body.Close())
			}
			confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolMergePerson, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": true}}}})
			requirements.NoError(err)
			if name != "commit and replay" {
				assertions.True(confirmed.IsError)
				if name == "revoke before confirmation" {
					assertions.Equal(uint64(1), mergePosts.Load())
					assertions.Contains(mcpDraftResultText(t, confirmed), "daemon request failed (401)")
				} else {
					assertions.Equal(uint64(0), mergePosts.Load())
				}
				current, err = fixture.store.GetPerson(survivor.ID)
				requirements.NoError(err)
				assertions.Equal(survivor, current)
				current, err = fixture.store.GetPerson(absorbed.ID)
				requirements.NoError(err)
				assertions.Equal(absorbed, current)
				return
			}
			requirements.False(confirmed.IsError, mcpDraftResultText(t, confirmed))
			assertions.Equal(uint64(1), mergePosts.Load())
			encoded, err = json.Marshal(confirmed.StructuredContent)
			requirements.NoError(err)
			var receipt generated.PersonMergeResult
			requirements.NoError(json.Unmarshal(encoded, &receipt))
			assertions.Equal("agent:"+grant.ID, receipt.Merge.Actor)
			assertions.Equal(survivor.ID, receipt.Person.ID)
			replayPending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolMergePerson, Arguments: args})
			requirements.NoError(err)
			requirements.True(replayPending.NeedsInput())
			replay, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolMergePerson, Arguments: args, RequestState: replayPending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
			requirements.NoError(err)
			requirements.False(replay.IsError, mcpDraftResultText(t, replay))
			replayEncoded, err := json.Marshal(replay.StructuredContent)
			requirements.NoError(err)
			assertions.JSONEq(string(encoded), string(replayEncoded))
			assertions.Equal(uint64(2), mergePosts.Load())
		})
	}
}
