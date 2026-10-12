package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

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
	seedArchivedIMAPInboxReads(t, f.store, f.source.ID)
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			response := httptest.NewRecorder()
			next.ServeHTTP(response, r)
			t.Logf("native diagnostic: %s %s status=%d", r.Method, r.URL.Path, response.Code)
			for key, values := range response.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
		})
	})
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

// Seed only the archived metadata contract. IMAP control/ingestion is a separate
// provider consumer; these read tests must not depend on it being implemented.
func seedArchivedIMAPInboxReads(t *testing.T, st *store.Store, sourceID int64) {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`SELECT member.message_id,m.source_message_id,member.mailbox,member.uidvalidity,member.uid,s.identifier
		FROM imap_message_memberships member JOIN messages m ON m.id=member.message_id AND m.source_id=member.source_id
		JOIN sources s ON s.id=member.source_id WHERE member.source_id=? AND member.mailbox='INBOX'`), sourceID)
	require.NoError(t, err)
	var states []inboxcontrol.State
	for rows.Next() {
		target := inboxcontrol.Target{SourceID: sourceID, SourceType: "imap", Scope: inboxcontrol.ScopeMessage}
		require.NoError(t, rows.Scan(&target.ItemID, &target.ProviderID, &target.Mailbox, &target.UIDValidity, &target.UID, &target.SourceIdentifier))
		identifier, err := url.Parse(target.SourceIdentifier)
		require.NoError(t, err)
		require.NotNil(t, identifier.User)
		target.AccountID = identifier.User.Username()
		states = append(states, inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()})
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for _, state := range states {
		key, err := inboxcontrol.SemanticFingerprint(inboxcontrol.State{Target: state.Target})
		require.NoError(t, err)
		hash, err := inboxcontrol.SemanticFingerprint(state)
		require.NoError(t, err)
		encoded, err := json.Marshal(state)
		require.NoError(t, err)
		target := state.Target
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO inbox_provider_states
			(target_key,source_id,scope,item_id,provider_id,mailbox,uidvalidity,uid,is_inbox,provider_read,marked_unread,semantic_hash,state_json,observed_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(target_key) DO NOTHING`), key, target.SourceID, target.Scope, target.ItemID, target.ProviderID, target.Mailbox, target.UIDValidity, target.UID, state.Inbox, state.Read, state.MarkedUnread, hash, string(encoded), state.ObservedAt.UnixNano())
		require.NoError(t, err)
	}
}
