package query_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/calendarfixture"
)

func TestEmailCalendarSearchAndDrillDown(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:search@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Orchid planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "abc123", MessageType: "email", Subject: sql.NullString{String: "Invitation", Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "abc123", Title: "Invitation"},
		RawMIME:      calendarfixture.EmailFrom("organizer@example.com", raw),
	})
	requirements.NoError(err)
	eng := query.NewSQLiteEngine(st.DB())
	for _, text := range []string{"Orchid", "Orchid message_type:calendar_event"} {
		rows, err := eng.Search(t.Context(), search.ParseOR(text), 50, 0)
		requirements.NoError(err)
		assertions.Len(rows, 1)
		rows, err = eng.SearchFast(t.Context(), search.ParseOR(text), query.MessageFilter{}, 50, 0)
		requirements.NoError(err)
		assertions.Len(rows, 1)
	}
	filter := query.MessageFilter{MessageType: "calendar_event"}
	rows, err := eng.ListMessages(t.Context(), filter)
	requirements.NoError(err)
	assertions.Len(rows, 1)
	targets, err := eng.GetDeletionTargetsByFilter(t.Context(), filter)
	requirements.NoError(err)
	assertions.Empty(targets)
	targets, err = eng.GetDeletionTargetsBySearch(t.Context(), search.ParseOR("Orchid"), query.MessageFilter{}, query.DeletionSearchDeep)
	requirements.NoError(err)
	assertions.Empty(targets)
}

func TestEmailCalendarStatsMatchListing(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:stats@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Orchid planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "stats123", MessageType: "email", Subject: sql.NullString{String: "Invitation", Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "stats123", Title: "Invitation"},
		RawMIME:      calendarfixture.EmailFrom("organizer@example.com", raw),
	})
	requirements.NoError(err)
	eng := query.NewSQLiteEngine(st.DB())
	filter := query.MessageFilter{MessageType: "calendar_event"}
	rows, err := eng.ListMessages(t.Context(), filter)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	stats, err := eng.GetTotalStats(t.Context(), query.StatsOptions{Filter: &filter})
	requirements.NoError(err)
	assertions.Equal(int64(len(rows)), stats.MessageCount)
}
