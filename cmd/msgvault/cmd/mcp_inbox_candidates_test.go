package cmd

import (
	"encoding/json/v2"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

// Exercises production discovery, MCP SDK, daemon transport and the real Store.
func TestMCPInboxCandidatesRealDaemonScopeAndCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftReplyFixture(t)
	conv, err := f.store.EnsureConversation(f.source.ID, "synthetic-inbox", "Synthetic inbox")
	requirements.NoError(err)
	_, err = f.store.UpsertMessage(&store.Message{SourceID: f.source.ID, ConversationID: conv, SourceMessageID: "INBOX|10", MessageType: "email"})
	requirements.NoError(err)
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 11}, Reset: true, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: 77, UID: 9, SourceMessageID: "INBOX|9", Flags: []string{}}, {Mailbox: "INBOX", UIDValidity: 77, UID: 10, SourceMessageID: "INBOX|10", Flags: []string{}}}}}))
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
	session := mcpDraftTestSession(mcpDraftAgentContext(t, server, f.source.ID, []string{"inbox.read"}), t)
	requirements.Contains(mcpDraftToolNames(t, session), "inbox_candidates")
	source := map[string]any{"source_id": f.source.ID, "source_type": "imap", "source_identifier": f.source.Identifier, "account_id": f.config.Username}
	args := map[string]any{"source": source, "scope": "message", "limit": 1}
	call := func() *sdkmcp.CallToolResult {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_candidates", Arguments: args})
		require.NoError(t, err)
		return result
	}
	decode := func(result *sdkmcp.CallToolResult) inboxcontrol.CandidatePage {
		require.False(t, result.IsError, mcpDraftResultText(t, result))
		data, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		var response struct {
			Page *inboxcontrol.CandidatePage `json:"page"`
		}
		require.NoError(t, json.Unmarshal(data, &response))
		require.NotNil(t, response.Page)
		return *response.Page
	}
	first := decode(call())
	requirements.Len(first.Candidates, 1)
	requirements.NotEmpty(first.NextCursor)
	args["cursor"] = first.NextCursor
	second := decode(call())
	requirements.Len(second.Candidates, 1)
	assertions.Empty(second.NextCursor)
	assertions.NotEqual(first.Candidates[0].State.Target, second.Candidates[0].State.Target)
	_, err = f.store.UpsertMessage(&store.Message{SourceID: f.source.ID, ConversationID: conv, SourceMessageID: "INBOX|11", MessageType: "email"})
	requirements.NoError(err)
	conflict := call()
	assertions.True(conflict.IsError)
	assertions.Contains(mcpDraftResultText(t, conflict), "conflict")
	delete(args, "cursor")
	source["account_id"] = "foreign@example.test"
	denied := call()
	assertions.True(denied.IsError)
	assertions.Contains(mcpDraftResultText(t, denied), "denied")
	source["account_id"] = f.config.Username
	source["source_id"] = float64(9007199254740992)
	assertions.True(call().IsError)
	source["source_id"] = f.source.ID
	delete(args, "limit")
	full := decode(call())
	assertions.Len(full.Candidates, 2)
	assertions.True(full.Unavailable)
	args["unexpected"] = true
	assertions.True(call().IsError)
	delete(args, "unexpected")
	args["limit"] = 101
	assertions.True(call().IsError)
	// The independent caller gets its own server/limiter; the earlier cursor
	// assertions deliberately make enough requests to consume a burst budget.
	deniedServer := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
	ungranted := mcpDraftTestSession(mcpDraftAgentContext(t, deniedServer, f.source.ID, []string{"draft.create"}), t)
	assertions.NotContains(mcpDraftToolNames(t, ungranted), "inbox_candidates")
}
