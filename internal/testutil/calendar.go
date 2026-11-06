package testutil

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// CalendarArchive creates two calendars with one event each.
func CalendarArchive(tb testing.TB, st *store.Store) (sourceIDs, messageIDs []int64) {
	tb.Helper()
	for _, address := range []string{"calendar0@example.com", "calendar1@example.com"} {
		source, err := st.GetOrCreateSource("gcal", address)
		require.NoError(tb, err)
		sourceIDs = append(sourceIDs, source.ID)
		data := &store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: "event", MessageType: "calendar_event"},
			Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar", Title: "Planning"},
			RawMIME:      []byte(`{"id":"event","status":"confirmed","location":"Example room","description":"Agenda","hangoutLink":"https://meet.example.com/event","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`), RawFormat: "gcal_json",
		}
		id, err := st.PersistMessage(data)
		require.NoError(tb, err)
		messageIDs = append(messageIDs, id)
	}
	return sourceIDs, messageIDs
}
