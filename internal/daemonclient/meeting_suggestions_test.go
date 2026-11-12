package daemonclient_test

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSuggestMeetingLinksDefaultLimit(t *testing.T) {
	r := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	r.NoError(err)
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	note, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "note", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: at, Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
	r.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	r.NoError(err)
	for i := range 11 {
		id := fmt.Sprintf("event-%d", i)
		_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: calendar.ID, SourceMessageID: id, MessageType: "calendar_event"}, Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "calendar"}, RawMIME: []byte(fmt.Sprintf(`{"id":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T10:30:00Z"}}`, id)), RawFormat: "gcal_json"})
		r.NoError(err)
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	r.NoError(err)
	t.Cleanup(func() { r.NoError(client.Close()) })
	page, err := client.SuggestMeetingLinks(t.Context(), fmt.Sprintf("msg:%d", note), 0)
	r.NoError(err)
	r.Len(page.Suggestions, 10)
}
