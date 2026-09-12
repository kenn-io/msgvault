package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type meetingQueryFixture struct {
	store        *Store
	sourceOne    int64
	sourceTwo    int64
	organizer    int64
	attendee     int64
	meetingIDs   []int64
	undatedID    int64
	localDelete  int64
	missingProj  int64
	nonMeetingID int64
}

func newMeetingQueryFixture(t *testing.T) *meetingQueryFixture {
	t.Helper()
	st := newRFC822IDBackfillBackendStore(t)
	sourceOne, err := st.GetOrCreateSource("meeting_import", "first@example.test")
	require.NoError(t, err)
	sourceTwo, err := st.GetOrCreateSource("granola", "second@example.test")
	require.NoError(t, err)
	organizer, err := st.EnsureParticipant("organizer@example.test", "Organizer", "example.test")
	require.NoError(t, err)
	attendee, err := st.EnsureParticipant("current@example.test", "Archived Attendee", "example.test")
	require.NoError(t, err)

	fixture := &meetingQueryFixture{
		store:     st,
		sourceOne: sourceOne.ID,
		sourceTwo: sourceTwo.ID,
		organizer: organizer,
		attendee:  attendee,
	}
	fixture.meetingIDs = []int64{
		fixture.persistMeeting(t, sourceOne.ID, "same-external-id", "Provider duration",
			new(time.Date(2026, time.January, 10, 10, 0, 0, 0, time.UTC)),
			`{"summary_text":"Provider summary","started_at":"2026-01-05T09:00:00Z","ended_at":"2026-01-05T09:30:00Z","recording_url":"https://signed.example.test/token","organizer":{"name":"Organizer","email":"Archived-Organizer@Example.test"},"attendees":[{"name":"Organizer","email":"Archived-Organizer@Example.test"},{"name":"Unresolved guest"}],"action_items":[{"source_id":"provider-0","title":"Send 100% plan","description":"Literal percent","assignee_name":"Archived Attendee","assignee_email":"ALIAS@Example.test","status":"open","due_date":"next week"},{"source_id":"provider-1","title":"Review_under_score","description":"Literal underscore","assignee_name":"Name only","status":"done"}]}`),
		fixture.persistMeeting(t, sourceTwo.ID, "same-external-id", "Scheduled duration",
			new(time.Date(2026, time.January, 5, 9, 0, 0, 0, time.UTC)),
			`{"summary_markdown":"Scheduled summary","calendar_event":{"scheduled_start_time":"2026-01-10T10:00:00Z","scheduled_end_time":"2026-01-10T11:00:00Z","organiser":"organizer@example.test"},"attendees":[{"name":"Organizer","email":"organizer@example.test"}],"transcript":[]}`),
		fixture.persistMeeting(t, sourceOne.ID, "transcript-span", "Transcript duration",
			new(time.Date(2026, time.January, 10, 10, 0, 0, 0, time.UTC)),
			`{"summary_text":"Transcript summary","transcript_segments":[{"speaker":"One","text":"Start","offset_seconds":0},{"speaker":"Two","text":"Finish","offset_seconds":600}],"action_items":[{"source_id":"span-0","title":"Wildcard %_ literal","assignee_email":"alias@example.test","status":"in_progress"},{}]}`),
		fixture.persistMeeting(t, sourceOne.ID, "unknown-duration", "Unknown duration",
			new(time.Date(2026, time.February, 2, 8, 0, 0, 0, time.UTC)),
			`{"summary_text":"Unknown summary","action_items":null}`),
	}
	fixture.undatedID = fixture.persistMeeting(t, sourceOne.ID, "undated", "Undated meeting", nil,
		`{"summary_text":"Undated summary","action_items":[{"source_id":"undated-0","title":"Undated follow-up","status":"pending"}]}`)
	fixture.localDelete = fixture.persistMeeting(t, sourceOne.ID, "local-delete", "Locally deleted", nil,
		`{"started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:01:00Z","action_items":[]}`)
	fixture.missingProj = fixture.persistMeeting(t, sourceOne.ID, "missing-projection", "Missing projection", nil,
		`{"summary_text":"Will lose projection","action_items":[]}`)
	fixture.nonMeetingID = fixture.persistMessage(t, sourceOne.ID, "email-row", "email", "Ordinary message", nil, nil, "")
	_, err = st.db.Exec(`UPDATE messages SET sent_at = NULL, received_at = ? WHERE id = ?`,
		time.Date(2026, time.January, 5, 9, 0, 0, 0, time.UTC), fixture.meetingIDs[1])
	require.NoError(t, err)
	_, err = st.db.Exec(`UPDATE messages SET sent_at = NULL, internal_date = ? WHERE id = ?`,
		time.Date(2026, time.January, 10, 10, 0, 0, 0, time.UTC), fixture.meetingIDs[2])
	require.NoError(t, err)

	_, err = st.db.Exec(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, fixture.meetingIDs[3])
	require.NoError(t, err)
	_, err = st.db.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, fixture.localDelete)
	require.NoError(t, err)
	_, err = st.db.Exec(`DELETE FROM meeting_details WHERE message_id = ?`, fixture.missingProj)
	require.NoError(t, err)
	return fixture
}

func (f *meetingQueryFixture) persistMeeting(
	t *testing.T, sourceID int64, sourceMessageID, title string, occurredAt *time.Time, raw string,
) int64 {
	t.Helper()
	return f.persistMessage(t, sourceID, sourceMessageID, "meeting_transcript", title, occurredAt,
		[]RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{f.organizer},
			DisplayNames:   []string{"Archived Organizer"},
			EmailAddresses: []string{"Archived-Organizer@Example.test"},
		}, {
			Type:           "to",
			ParticipantIDs: []int64{f.organizer, f.attendee},
			DisplayNames:   []string{"Organizer attendee", "Archived Attendee"},
			EmailAddresses: []string{"organizer@example.test", "Alias@Example.test"},
		}}, raw)
}

func (f *meetingQueryFixture) persistMessage(
	t *testing.T, sourceID int64, sourceMessageID, messageType, title string, occurredAt *time.Time,
	recipients []RecipientSet, raw string,
) int64 {
	t.Helper()
	var sentAt sql.NullTime
	if occurredAt != nil {
		sentAt = sql.NullTime{Time: occurredAt.UTC(), Valid: true}
	}
	data := &MessagePersistData{
		Message: &Message{
			SourceID:        sourceID,
			SourceMessageID: sourceMessageID,
			MessageType:     messageType,
			SentAt:          sentAt,
			SenderID:        sql.NullInt64{Int64: f.organizer, Valid: true},
			Subject:         sql.NullString{String: title, Valid: true},
		},
		Conversation: &ConversationPersistData{
			SourceConversationID: fmt.Sprintf("conversation-%d-%s", sourceID, sourceMessageID),
			ConversationType:     "meeting",
			Title:                title,
			Participants: []ConversationParticipantRef{
				{ParticipantID: f.organizer, Role: "owner"},
				{ParticipantID: f.attendee, Role: "member"},
			},
		},
		Recipients: recipients,
	}
	if raw != "" {
		data.RawMIME = []byte(raw)
		data.RawFormat = "meeting_json"
		if sourceID == f.sourceTwo {
			data.RawFormat = "granola_json"
		}
	}
	id, err := f.store.PersistMessage(data)
	require.NoError(t, err)
	return id
}
