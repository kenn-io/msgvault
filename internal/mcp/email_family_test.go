package mcp

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestEmailFamilyCatalogAndReads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("imap", "family-mcp@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n")})
	require.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, EmailFamilyBackend: st}
	listed := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(listed, "get_email_family")
	assert.Equal(true, toolReadOnlyHint(t, listed["get_email_family"]))
	response := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "get_email_family", "arguments": map[string]any{"message_id": id}})
	content := toolStructuredContent(t, response.Result)
	assert.EqualValues(id, content["anchor_message_id"])
	delegated := ServeOptions{
		Engine:             &querytest.MockEngine{},
		EmailFamilyBackend: st,
		DelegatedOnly:      true,
		GrantPermissions:   []string{string(agentgrant.PermissionMessageRead)},
	}
	delegatedListed := toolsByName(t, rawListTools(t, delegated, false))
	require.Contains(delegatedListed, ToolGetMessage, "message.read must expose a supported message read")
	assert.NotContains(delegatedListed, "get_email_family", "message.read must not expose the owner-only family read")
}

func TestEmailFamilyDecisionToolsNeedWriteOptInAndConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, EmailFamilyBackend: st, EmailFamilyDecisions: st}
	read := toolsByName(t, rawListTools(t, opts, false))
	assert.NotContains(read, "apply_email_family_decision")
	assert.NotContains(read, "preview_email_family_decision")
	write := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(write, "apply_email_family_decision")
	require.Contains(write, "undo_email_family_decision")
	assert.Equal(false, toolReadOnlyHint(t, write["apply_email_family_decision"]))
	h := &handlers{emailFamilyDecisions: st}
	result, err := h.applyEmailFamilyDecision(t.Context(), toolRequest{arguments: map[string]any{"kind": "handled", "message_ids": []any{float64(1)}, "request_key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "expected_family_revision": "token"}})
	require.NoError(err)
	require.NotNil(result)
	assert.True(result.isError)
	assert.Contains(result.text, "confirmation")
	var count int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM email_family_decisions").Scan(&count))
	assert.Zero(count)
	opts.DelegatedOnly = true
	assert.NotContains(toolsByName(t, rawListTools(t, opts, true)), "apply_email_family_decision")
}

func TestEmailFamilyDecisionMCPPreviewConfirmAndUndo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("imap", "decisions-mcp@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n")})
	require.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, EmailFamilyBackend: st, EmailFamilyDecisions: st}
	args := map[string]any{"kind": "handled", "message_ids": []int64{id}, "request_key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	preview := rawModernCall(t, opts, HTTPOptions{AllowWrites: true}, "tools/call", map[string]any{"name": "preview_email_family_decision", "arguments": args})
	content := toolStructuredContent(t, preview.Result)
	revision, ok := content["before_revision"].(string)
	require.True(ok)
	require.NotEmpty(revision)
	args["expected_family_revision"] = revision
	handler := newMCPHTTPServer(opts, HTTPOptions{APIKey: "test-owner", AllowWrites: true}).Handler
	call := func(name string, args map[string]any, state any) task3RPCResponse {
		t.Helper()
		params := map[string]any{"name": name, "arguments": args, "_meta": modernRequestMeta()}
		if state != nil {
			params["requestState"] = state
			params["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		require.NoError(err)
		request := task3ModernRequest("tools/call", name, string(body))
		request.Header.Set("Authorization", "Bearer test-owner")
		recorder, response := task3Serve(handler, request)
		task3RequireSuccess(t, recorder, response)
		return response
	}
	pending := call("apply_email_family_decision", args, nil)
	require.NotEmpty(pending.Result["requestState"])
	var count int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM email_family_decisions").Scan(&count))
	assert.Zero(count)
	applied := call("apply_email_family_decision", args, pending.Result["requestState"])
	result := toolStructuredContent(t, applied.Result)
	encoded, err := json.Marshal(result)
	require.NoError(err)
	var saved store.EmailFamilyDecisionResult
	require.NoError(json.Unmarshal(encoded, &saved))
	assert.Equal("active", saved.Decision.State)
	require.Len(saved.Family.Members, 1)
	assert.True(saved.Family.Members[0].LocalHandled)
	undo := map[string]any{"decision_id": saved.Decision.ID, "expected_revision": saved.Decision.Revision, "expected_family_revision": saved.Family.Revision}
	pending = call("undo_email_family_decision", undo, nil)
	require.NotEmpty(pending.Result["requestState"])
	undone := call("undo_email_family_decision", undo, pending.Result["requestState"])
	result = toolStructuredContent(t, undone.Result)
	encoded, err = json.Marshal(result)
	require.NoError(err)
	require.NoError(json.Unmarshal(encoded, &saved))
	assert.Equal("undone", saved.Decision.State)
	assert.False(saved.Family.Members[0].LocalHandled)
}
