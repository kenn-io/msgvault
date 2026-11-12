package daemonclient_test

import (
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSuggestMeetingLinksDefaultLimit(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	require.NoError(t, err)
	note, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "note", MessageType: "meeting_transcript"}, Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
	require.NoError(t, err)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	page, err := client.SuggestMeetingLinks(t.Context(), fmt.Sprintf("msg:%d", note), 0)
	require.NoError(t, err)
	require.Empty(t, page.Suggestions)
}
