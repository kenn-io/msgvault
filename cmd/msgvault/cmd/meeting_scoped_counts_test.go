package cmd

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMeetingCountsPendingSourceScope(t *testing.T) {
	dir := t.TempDir()
	dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	calendar, err := st.GetOrCreateSource("gcal", "owner@example.com")
	require.NoError(t, err)
	notes, err := st.GetOrCreateSource("meeting_import", "owner@example.com")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentity(calendar.ID, "owner@example.com", "manual"))
	require.NoError(t, st.AddAccountIdentity(notes.ID, "owner@example.com", "manual"))
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, err)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	at := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	makeMessage := func(sourceID int64, key, typ, format, raw string) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: key, MessageType: typ, Subject: sql.NullString{String: key, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"},
			RawMIME:      []byte(raw), RawFormat: format,
		})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
		require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
		return id
	}
	event := makeMessage(calendar.ID, "event", "calendar_event", "gcal_json", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
	note := makeMessage(notes.ID, "note", "meeting_transcript", "meeting_json", `{}`)
	_, err = st.DecideMeetingLinkContext(t.Context(), store.MeetingLinkRef{Kind: "evt", MessageID: event}, store.MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
	require.NoError(t, err)
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	for _, tc := range []struct {
		name  string
		ids   []int64
		types []string
		want  int64
	}{
		{"calendar", []int64{calendar.ID}, nil, 1},
		{"notes", []int64{notes.ID}, nil, 0},
		{"notes_type", nil, []string{"meeting_transcript"}, 0},
		{"both", []int64{calendar.ID, notes.ID}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := query.Context{SourceIDs: tc.ids, MessageTypes: tc.types}
			detail, err := engine.GetPerson(t.Context(), person, scope, nil)
			require.NoError(t, err)
			if detail != nil {
				assert.Equal(t, tc.want, detail.MeetingCount)
			} else {
				assert.Zero(t, tc.want)
			}
			people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Explore: query.ExploreRequest{Context: scope}})
			require.NoError(t, err)
			found := false
			for _, p := range people.Rows {
				if p.ID == person {
					found = true
					assert.Equal(t, tc.want, p.MeetingCount)
				}
			}
			if tc.want > 0 {
				assert.True(t, found)
			}
			relationships, err := engine.Relationships(t.Context(), query.RelationshipsRequest{Context: scope, ShowAll: true})
			require.NoError(t, err)
			found = false
			for _, p := range relationships.Rows {
				if p.CanonicalID == person {
					found = true
					assert.Equal(t, tc.want, p.Signals.MeetingCount)
				}
			}
			if tc.want > 0 {
				assert.True(t, found)
			}
		})
	}
	available := makeMessage(notes.ID, "available", "meeting_transcript", "meeting_json", `{"transcript":"Notes"}`)
	second := makeMessage(notes.ID, "second", "meeting_transcript", "meeting_json", `{"transcript":"More notes"}`)
	for _, id := range []int64{available, second} {
		_, err = st.DecideMeetingLinkContext(t.Context(), store.MeetingLinkRef{Kind: "evt", MessageID: event}, store.MeetingLinkRef{Kind: "msg", MessageID: id}, "link")
		require.NoError(t, err)
		require.NoError(t, st.UpsertAttachment(t.Context(), id, fmt.Sprintf("note-%d.txt", id), "text/plain", "", "", 10))
		_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET attachment_count=1, has_attachments=1 WHERE id=?`, id)
		require.NoError(t, err)
	}
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	updated, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, updated.Close()) }()
	scope := query.Context{SourceIDs: []int64{calendar.ID}}
	detail, err := updated.GetPerson(t.Context(), person, scope, nil)
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.EqualValues(t, 2, detail.FileCount)
	timeline, err := updated.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: person, Timezone: "UTC", Context: scope})
	require.NoError(t, err)
	require.Len(t, timeline.Rows, 1)
	assert.Equal(t, calendar.ID, timeline.Rows[0].SourceID)
	require.NotNil(t, timeline.Rows[0].AnchorMessageID)
	assert.Equal(t, event, *timeline.Rows[0].AnchorMessageID)
	notesTimeline, err := updated.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: person, Timezone: "UTC", Context: query.Context{SourceIDs: []int64{notes.ID}}})
	require.NoError(t, err)
	assert.Empty(t, notesTimeline.Rows)
}
