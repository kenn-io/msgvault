package granola

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportDateOnlyMeetingSuggestions(t *testing.T) {
	r := require.New(t)
	raw := []byte(`{"id":"note-date","title":"Planning","created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-01T00:00:00Z","owner":{"email":"alice@example.com"},"calendar_event":{"scheduled_start_time":"2026-06-01","scheduled_end_time":"2026-06-02"}}`)
	imp, st := newTestImporter(t, &fakeAPI{notes: map[string][]byte{"note-date": raw}})
	result, err := imp.Import(t.Context(), ImportOptions{Identifier: "alice@example.com", AccountEmail: "alice@example.com"})
	r.NoError(err)
	r.EqualValues(1, result.NotesAdded)
	var note int64
	r.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'note-date'`).Scan(&note))
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	r.NoError(err)
	r.NoError(st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"calendar_id":"calendar@example.com","time_zone":"UTC"}`))
	event, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "date-event", MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "date-event", ConversationType: "calendar", Title: "Planning"},
		RawMIME:      []byte(`{"id":"date-event","start":{"dateTime":"2026-06-01T00:00:00Z"},"end":{"dateTime":"2026-06-01T01:00:00Z"}}`), RawFormat: "gcal_json",
	})
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), store.MeetingLinkRef{Kind: "msg", OccurrenceRef: store.OccurrenceRef{MessageID: note}}, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(60, page.Suggestions[0].OverlapMinutes)
	page, err = st.SuggestMeetingLinksContext(t.Context(), store.MeetingLinkRef{Kind: "evt", OccurrenceRef: store.OccurrenceRef{MessageID: event}}, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(note, page.Suggestions[0].MemberMessageID)
	r.Equal(60, page.Suggestions[0].OverlapMinutes)
}
