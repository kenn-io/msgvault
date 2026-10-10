package mcp

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPFindChatRealArchiveAndCapability(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	source, err := st.GetOrCreateSource("beeper", "fixture-account")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(source.ID, "Beeper iMessage"))
	unrelated, err := st.EnsureConversationWithType(source.ID, "unrelated-chat", "direct_chat", "Other")
	requirements.NoError(err)
	for _, providerID := range []string{"unrelated-one", "unrelated-two"} {
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: providerID, ConversationID: unrelated, MessageType: "beeper"})
		requirements.NoError(err)
	}
	chat, err := st.EnsureConversationWithType(source.ID, "provider-chat", "direct_chat", "Lee Chen")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "fixture-message", ConversationID: chat, MessageType: "beeper", Snippet: sql.NullString{String: "Synthetic", Valid: true}})
	requirements.NoError(err)
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "fixture-key"}}, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(daemon.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "fixture-key", AllowInsecure: true})
	requirements.NoError(err)
	opts := ServeOptions{Engine: daemonclient.NewEngineAdapter(client), ChatDiscoveryBackend: client}
	tools := toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(tools, "find_chat")
	assertions.NotContains(toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, true)), "find_chat")
	delegated := opts
	delegated.DelegatedOnly = true
	assertions.NotContains(toolsByName(t, rawListTools(t, delegated, true)), "find_chat")
	for _, tc := range []struct{ sourceType, account, provider string }{{"beeper", "", ""}, {"apple_messages", "fixture-account", "provider-chat"}} {
		other, err := st.GetOrCreateSource(tc.sourceType, tc.account)
		requirements.NoError(err)
		conversation, err := st.EnsureConversationWithType(other.ID, tc.provider, "direct_chat", "Anchor Case")
		requirements.NoError(err)
		anchor, err := st.UpsertMessage(&store.Message{SourceID: other.ID, SourceMessageID: "anchor-case", ConversationID: conversation, MessageType: "beeper"})
		requirements.NoError(err)
		found := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "find_chat", "arguments": map[string]any{"query": "Anchor Case", "source_id": other.ID}})
		requirements.Empty(found.Error)
		encoded, err := json.Marshal(toolStructuredContent(t, found.Result))
		requirements.NoError(err)
		var candidates store.ChatDiscoveryPage
		requirements.NoError(json.Unmarshal(encoded, &candidates))
		requirements.Len(candidates.Results, 1)
		assertions.Equal(anchor, candidates.Results[0].MessageID)
		read := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "list_thread", "arguments": map[string]any{"id": candidates.Results[0].MessageID}})
		requirements.Empty(read.Error)
		assertions.NotEqual(true, read.Result["isError"])
		encoded, err = json.Marshal(toolStructuredContent(t, read.Result))
		requirements.NoError(err)
		var opened struct {
			ConversationID int64 `json:"conversation_id"`
			Messages       []struct {
				ID int64 `json:"id"`
			} `json:"messages"`
		}
		requirements.NoError(json.Unmarshal(encoded, &opened))
		assertions.Equal(conversation, opened.ConversationID)
		requirements.Len(opened.Messages, 1)
		assertions.Equal(anchor, opened.Messages[0].ID)
	}

	for _, args := range []map[string]any{{"query": "Lee", "limit": 0}, {"query": "Lee", "limit": -1}, {"query": "Lee", "limit": 101}, {"query": "Lee", "source_id": 0}, {"query": "Lee", "extra": true}} {
		invalid := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "find_chat", "arguments": args})
		assertions.True(len(invalid.Error) > 0 || invalid.Result["isError"] == true, "args: %#v, response: %#v", args, invalid)
	}
}
