package cmd

import (
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

// Exercise real startup discovery, SDK tool admission, scoped native HTTP,
// direct Store content reads, and preservation of committed read markers.
func TestMCPInboxContextRealDaemonContentGrants(t *testing.T) {
	for _, test := range []struct {
		name        string
		permissions []string
		admitted    bool
	}{
		{"metadata-only", []string{"inbox.read"}, false},
		{"content-only", []string{"inbox.content-read"}, false},
		{"both", []string{"inbox.read", "inbox.content-read"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := newDraftReplyFixture(t)
			requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 10}, Reset: true, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: 77, UID: 9, SourceMessageID: "INBOX|9", Flags: []string{}}}}}))
			target := inboxcontrol.Target{SourceID: f.source.ID, SourceType: "imap", SourceIdentifier: f.source.Identifier, AccountID: f.config.Username, Scope: inboxcontrol.ScopeMessage, ItemID: f.parentID, ProviderID: "INBOX|9", Mailbox: "INBOX", UIDValidity: 77, UID: 9}
			before, err := f.store.GetInboxProviderState(t.Context(), target)
			requirements.NoError(err)
			requirements.NotNil(before)
			var contextCalls atomic.Int32
			server := mcpDraftTestDaemon(t, f.grantedAdapter(), func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/inbox/context" {
						contextCalls.Add(1)
					}
					next.ServeHTTP(w, r)
				})
			})
			session := mcpDraftTestSession(mcpDraftAgentContext(t, server, f.source.ID, test.permissions), t)
			names := mcpDraftToolNames(t, session)
			if !test.admitted {
				assertions.NotContains(names, "inbox_context")
				return
			}
			requirements.Contains(names, "inbox_context")
			targetJSON, err := json.Marshal(target)
			requirements.NoError(err)
			var targetArgs map[string]any
			requirements.NoError(json.Unmarshal(targetJSON, &targetArgs))
			args := map[string]any{"target": targetArgs, "max_bytes": 6}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_context", Arguments: args})
			requirements.NoError(err)
			requirements.False(result.IsError, mcpDraftResultText(t, result))
			encoded, err := json.Marshal(result.StructuredContent)
			requirements.NoError(err)
			var response struct {
				Context *inboxcontrol.Context `json:"context"`
			}
			requirements.NoError(json.Unmarshal(encoded, &response))
			requirements.NotNil(response.Context)
			assertions.Equal(target, response.Context.Target)
			assertions.Equal(f.parentID, response.Context.MessageID)
			assertions.Equal("Parent", response.Context.Text)
			assertions.True(response.Context.Truncated)
			assertions.False(response.Context.Unavailable)
			after, err := f.store.GetInboxProviderState(t.Context(), target)
			requirements.NoError(err)
			assertions.Equal(before, after)
			assertions.Empty(*f.refreshed, "archive context must not refresh analytics or mutate providers")
			targetArgs["account_id"] = "foreign@example.test"
			denied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_context", Arguments: args})
			requirements.NoError(err)
			assertions.True(denied.IsError)
			assertions.Contains(mcpDraftResultText(t, denied), "denied")
			targetArgs["account_id"] = f.config.Username
			targetArgs["item_id"] = float64(9007199254740992)
			unsafeID, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_context", Arguments: args})
			requirements.NoError(err)
			assertions.True(unsafeID.IsError)
			assertions.Equal(int32(2), contextCalls.Load(), "unsafe IDs must be rejected before daemon context access")
		})
	}
}

func TestMCPInboxContextRealDaemonChatIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftReplyFixture(t)
	source, err := f.store.GetOrCreateSource("beeper", "synthetic-chat-account")
	requirements.NoError(err)
	chat, err := f.store.EnsureConversation(source.ID, "synthetic-chat", "Synthetic chat")
	requirements.NoError(err)
	mid, err := f.store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "synthetic-chat-message", MessageType: "beeper"})
	requirements.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "Synthetic chat body")
	requirements.NoError(err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "beeper", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeChat, ItemID: chat, ProviderID: "synthetic-chat"}
	_, err = f.store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), MarkedUnread: new(false), ObservedAt: time.Now().UTC()})
	requirements.NoError(err)
	before, err := f.store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(before)
	var contextCalls atomic.Int32
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/inbox/context" {
				contextCalls.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	session := mcpDraftTestSession(mcpDraftAgentContext(t, server, source.ID, []string{"inbox.read", "inbox.content-read"}), t)
	targetJSON, err := json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(targetJSON, &targetArgs))
	args := map[string]any{"target": targetArgs, "message_id": mid, "max_bytes": 9}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_context", Arguments: args})
	requirements.NoError(err)
	requirements.False(result.IsError, mcpDraftResultText(t, result))
	encoded, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var response struct {
		Context *inboxcontrol.Context `json:"context"`
	}
	requirements.NoError(json.Unmarshal(encoded, &response))
	requirements.NotNil(response.Context)
	assertions.Equal(mid, response.Context.MessageID)
	assertions.Equal("Synthetic", response.Context.Text)
	assertions.True(response.Context.Truncated)
	assertions.Equal(target, response.Context.Target)
	args["message_id"] = f.parentID
	foreign, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_context", Arguments: args})
	requirements.NoError(err)
	assertions.True(foreign.IsError)
	assertions.Contains(mcpDraftResultText(t, foreign), "denied")
	for _, messageID := range []any{nil, float64(9007199254740992)} {
		if messageID == nil {
			delete(args, "message_id")
		} else {
			args["message_id"] = messageID
		}
		invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_context", Arguments: args})
		requirements.NoError(err)
		assertions.True(invalid.IsError)
	}
	assertions.Equal(int32(2), contextCalls.Load(), "missing and unsafe selectors must not reach the daemon")
	after, err := f.store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	assertions.Equal(before, after)
	assertions.Empty(*f.refreshed)
}
