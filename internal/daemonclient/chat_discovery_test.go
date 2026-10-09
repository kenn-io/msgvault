package daemonclient_test

import (
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChatDiscoveryClientReadsRealDaemon(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "test-account")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(source.ID, "Beeper iMessage"))
	conversation, err := st.EnsureConversationWithType(source.ID, "provider-chat", "direct_chat", "Lee Chen")
	requirements.NoError(err)
	message, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, SourceMessageID: "fixture-message", ConversationID: conversation,
		MessageType: "beeper", Snippet: sql.NullString{String: "Synthetic message", Valid: true},
	})
	requirements.NoError(err)
	server := api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: "fixture-key"}}, st, nil, slog.New(slog.DiscardHandler))
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: httpServer.URL, APIKey: "fixture-key", AllowInsecure: true})
	requirements.NoError(err)
	page, err := client.SearchChats(t.Context(), store.ChatDiscoveryQuery{Query: "Jordan Lee Chen", Limit: 1, SourceID: source.ID})
	requirements.NoError(err)
	requirements.Len(page.Results, 1)
	assertions.Equal(conversation, page.Results[0].ConversationID)
	assertions.Equal(message, page.Results[0].MessageID)
	assertions.Equal("provider-chat", page.Results[0].SourceConversationID)
	assertions.Equal("iMessage", page.Results[0].Network)
	assertions.Equal([]string{"lee", "chen"}, page.Results[0].MatchedTokens)
	emptySource, err := st.GetOrCreateSource("beeper", "")
	requirements.NoError(err)
	emptyChat, err := st.EnsureConversationWithType(emptySource.ID, "", "direct_chat", "")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("peer@example.test", "Empty Metadata", "example.test")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: emptySource.ID, SourceMessageID: "empty-metadata", ConversationID: emptyChat,
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: peer, Valid: true}})
	requirements.NoError(err)
	page, err = client.SearchChats(t.Context(), store.ChatDiscoveryQuery{Query: "Empty Metadata", SourceID: emptySource.ID})
	requirements.NoError(err)
	requirements.Len(page.Results, 1)
	assertions.Equal(emptyChat, page.Results[0].ConversationID)
	assertions.Empty(page.Results[0].Title)
	assertions.Empty(page.Results[0].SourceDisplayName)
	assertions.Empty(page.Results[0].SourceConversationID)
	assertions.Empty(page.Results[0].SourceIdentifier)
	page, err = client.SearchChats(t.Context(), store.ChatDiscoveryQuery{Query: "Absent"})
	requirements.NoError(err)
	assertions.Equal([]store.ChatDiscoveryResult{}, page.Results)
	_, err = client.SearchChats(t.Context(), store.ChatDiscoveryQuery{Query: "Lee Chen", SourceID: -1})
	requirements.Error(err)
	assertions.Contains(err.Error(), "source_id must be positive")
}

func TestChatDiscoveryClientRejectsOlderDaemon(t *testing.T) {
	for _, version := range []string{"3.4.0", "3.10.0"} {
		t.Run(version, func(t *testing.T) {
			searchRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"` + version + `"}`))
					return
				}
				searchRequests++
				http.NotFound(w, r)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "fixture-key", AllowInsecure: true})
			require.NoError(t, err)
			_, err = client.SearchChats(t.Context(), store.ChatDiscoveryQuery{Query: "Lee Chen"})
			require.ErrorContains(t, err, "requires daemon API schema 3.11.0 or newer")
			assert.Zero(t, searchRequests)
		})
	}
}
