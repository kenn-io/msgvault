package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func timelineContactQuery(f *timelineActivityFixture) store.ContactQuery {
	return store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), Location: time.UTC}
}
func TestMessageAggregateCountsOnlyCountedInScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	f.add(t, f.ConvID, f.Source.ID, q.Now.AddDate(-2, 0, 0), false, "")
	f.add(t, f.ConvID, f.Source.ID, q.Now, true, "")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(2, got.Count)
	requirements.NotNil(got.FirstAt)
	assertions.Equal(q.Now.AddDate(-2, 0, 0), *got.FirstAt)
	assertions.Equal([]store.ActivityChannel{store.ChannelEmail}, got.Channels)
	got, err = f.Store.MessageContactAggregateContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), q)
	requirements.NoError(err)
	assertions.Zero(got.Count)
	assertions.Nil(got.FirstAt)
}
func TestMessageAggregateInitiationUsesSevenDayGap(t *testing.T) {
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for _, v := range []struct {
		days int
		out  bool
	}{{-30, false}, {-24, true}, {-17, true}, {-9, false}} {
		f.add(t, f.ConvID, f.Source.ID, q.Now.AddDate(0, 0, v.days), v.out, "")
	}
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 2, got.InitiatedByContact)
	assert.Equal(t, 1, got.InitiatedByOwner)
}
func TestMessageAggregateMonthlyRowEndsAtNow(t *testing.T) {
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for _, at := range []time.Time{time.Date(2025, 10, 31, 23, 0, 0, 0, time.UTC), time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC), q.Now, q.Now.AddDate(0, 1, 0)} {
		f.add(t, f.ConvID, f.Source.ID, at, false, "")
	}
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, [12]int{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}, got.Monthly)
}
func TestMessageAggregateUnsavedCluster(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1, got.Count)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	saved, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(got, saved)
}
func TestMessageAggregateIgnoresIncludeNoise(t *testing.T) {
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	id := f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), id)
	require.NoError(t, err)
	projectTimelineActivity(t, f, "UTC")
	q.Filters.IncludeNoise = true
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Zero(t, got.Count)
}
func TestAttendedTranscriptIDsByRole(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for i := range 3 {
		id := f.add(t, f.ConvID, f.Source.ID, q.Now.Add(time.Duration(i-3)*time.Minute), false, "")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), id)
		requirements.NoError(err)
	}
	projectTimelineActivity(t, f, "UTC")
	got, truncated, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 2)
	requirements.NoError(err)
	assertions.Len(got, 2)
	assertions.True(truncated)
	q.Subject.ParticipantIDs = []int64{f.owner}
	got, _, err = f.Store.AttendedTranscriptIDsContext(t.Context(), q, 2)
	requirements.NoError(err)
	assertions.Empty(got)
}

func TestAttendedTranscriptIDsResolveCalDAVUIDDecision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	const uid = "meeting@example.org"
	const raw = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:meeting@example.org\r\nDTSTART:20261021T170000Z\r\nDTEND:20261021T180000Z\r\nSUMMARY:Team meeting\r\nATTENDEE;CN=Alice:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	source, err := f.Store.GetOrCreateSource(calevent.CalDAVSourceType, "https://calendar.example.org/calendars/synthetic")
	requirements.NoError(err)
	components, err := calevent.ParseCalDAVResource(t.Context(), []byte(raw), "UTC")
	requirements.NoError(err)
	requirements.Len(components, 1)
	metadata, err := json.Marshal(calevent.CalDAVMetadata{
		EventID: components[0].Event.ProviderEventID, TimeZone: "UTC",
	}, json.Deterministic(true))
	requirements.NoError(err)
	_, err = f.Store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "team-meeting", MessageType: "calendar_event",
			Subject: sql.NullString{String: components[0].Summary, Valid: true},
		},
		Conversation: &store.ConversationPersistData{
			SourceConversationID: "team-meeting", ConversationType: "calendar", Title: components[0].Summary,
		},
		RawMIME: []byte(raw), RawFormat: calevent.CalDAVRawFormat,
		Metadata: &sql.NullString{String: string(metadata), Valid: true},
	})
	requirements.NoError(err)

	transcript := f.add(t, f.ConvID, f.Source.ID, q.Now.Add(-time.Hour), false, "")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), transcript)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,occurrence_key,decision,created_at) VALUES (?,?,'','link',?)`), transcript, uid, q.Now)
	requirements.NoError(err)
	projectTimelineActivity(t, f, "UTC")

	got, truncated, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
	requirements.NoError(err)
	assertions.False(truncated)
	assertions.Equal([]int64{transcript}, got)
}

func TestAttendedTranscriptIDsMatchICSUIDsAndGoogleInstanceIDs(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	const icsUID = "ics-meeting@example.test"
	icsSource, err := f.Store.GetOrCreateSource("ics", "calendar@example.test")
	requirements.NoError(err)
	requirements.NoError(f.Store.UpdateSourceSyncConfig(icsSource.ID, `{"time_zone":"UTC"}`))
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + icsUID + "\r\nDTSTART:20261021T170000Z\r\nDTEND:20261021T180000Z\r\nSUMMARY:Team meeting\r\nATTENDEE;CN=Alice:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	_, err = f.Store.PersistICalendarContext(t.Context(), icsSource.ID, []byte(ics))
	requirements.NoError(err)

	googleSource, err := f.Store.GetOrCreateSource("gcal", "google-calendar@example.test")
	requirements.NoError(err)
	requirements.NoError(f.Store.UpdateSourceSyncConfig(googleSource.ID, `{"calendar_id":"google-calendar@example.test","time_zone":"UTC"}`))
	google := `{"id":"series","iCalUID":"series@example.test","status":"confirmed","summary":"Team meeting","start":{"dateTime":"2026-10-20T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2026-10-20T11:00:00Z","timeZone":"UTC"},"recurrence":["RRULE:FREQ=DAILY;COUNT=5"],"attendees":[{"email":"alice@example.com","responseStatus":"accepted"}]}`
	metadata := `{"calendar_id":"google-calendar@example.test","ical_uid":"series@example.test"}`
	_, err = f.Store.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: googleSource.ID, SourceMessageID: "weekly-series", MessageType: "calendar_event", Subject: sql.NullString{String: "Team meeting", Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "weekly-series", ConversationType: "calendar", Title: "Team meeting"},
		RawMIME:      []byte(google), RawFormat: "gcal_json", Metadata: &sql.NullString{String: metadata, Valid: true},
	})
	requirements.NoError(err)

	addTranscript := func(name, kind, value string, decision bool) int64 {
		id := f.add(t, f.ConvID, f.Source.ID, q.Now.Add(-time.Hour), false, "")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), id)
		requirements.NoError(err)
		if decision {
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,occurrence_key,decision,created_at) VALUES (?,?,'','link',?)`), id, value, q.Now)
		} else {
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_key(message_id,kind,value,calendar_id) VALUES (?,?,?,'')`), id, kind, value)
		}
		requirements.NoError(err, name)
		return id
	}
	icsKey := addTranscript("ICS UID key", "ical_uid", icsUID, false)
	icsDecision := addTranscript("ICS UID decision", "", icsUID, true)
	googleInstance := addTranscript("Google instance", "google_event_id", "series_20261022T100000Z", false)
	projectTimelineActivity(t, f, "UTC")

	got, truncated, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
	requirements.NoError(err)
	assertions.False(truncated)
	assertions.ElementsMatch([]int64{icsKey, icsDecision, googleInstance}, got)
}

func TestAttendedTranscriptIDsCorrelateEventBeforeCandidateBudget(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	source, err := f.Store.GetOrCreateSource(calevent.CalDAVSourceType, "https://calendar.example.org/calendars/synthetic")
	requirements.NoError(err)
	persistCalendar := func(sourceMessageID, uid, attendee, start, end string) int64 {
		raw := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:%s\r\nDTSTART:%s\r\nDTEND:%s\r\nSUMMARY:Team meeting\r\nATTENDEE;CN=Alice:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", uid, start, end)
		raw = strings.Replace(raw, "alice@example.com", attendee, 1)
		components, err := calevent.ParseCalDAVResource(t.Context(), []byte(raw), "UTC")
		requirements.NoError(err)
		requirements.Len(components, 1)
		metadata, err := json.Marshal(calevent.CalDAVMetadata{EventID: components[0].Event.ProviderEventID, TimeZone: "UTC"}, json.Deterministic(true))
		requirements.NoError(err)
		id, err := f.Store.PersistMessage(&store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: sourceMessageID, MessageType: "calendar_event",
				Subject: sql.NullString{String: components[0].Summary, Valid: true},
			},
			Conversation: &store.ConversationPersistData{
				SourceConversationID: sourceMessageID, ConversationType: "calendar", Title: components[0].Summary,
			},
			RawMIME: []byte(raw), RawFormat: calevent.CalDAVRawFormat,
			Metadata: &sql.NullString{String: string(metadata), Valid: true},
		})
		requirements.NoError(err)
		return id
	}
	persistCalendar("past-meeting", "past@example.org", "alice@example.com", "20261021T170000Z", "20261021T180000Z")
	linkedCopy := persistCalendar("past-copy", "past@example.org", "bob@example.test", "20261021T170000Z", "20261021T180000Z")
	persistCalendar("unrelated-meeting", "unrelated@example.org", "bob@example.test", "20261021T170000Z", "20261021T180000Z")
	addTranscript := func(uid string, targetMessageID int64, at time.Time) int64 {
		id := f.add(t, f.ConvID, f.Source.ID, at, false, "")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript',sender_id=?,is_from_me=TRUE WHERE id=?`), f.owner, id)
		requirements.NoError(err)
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.owner}, []string{"Owner"}))
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner}, []string{"Owner"}))
		if targetMessageID > 0 {
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,target_message_id,occurrence_key,decision,created_at) VALUES (?,'',?,'','link',?)`), id, targetMessageID, q.Now)
		} else {
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,occurrence_key,decision,created_at) VALUES (?,?,'','link',?)`), id, uid, q.Now)
		}
		requirements.NoError(err)
		return id
	}
	wanted := addTranscript("past@example.org", 0, q.Now.Add(-24*time.Hour))
	aliased := addTranscript("", linkedCopy, q.Now.Add(-23*time.Hour))
	projectTimelineActivity(t, f, "UTC")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`WITH digits(n) AS (VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9))
INSERT INTO messages(conversation_id,source_id,source_message_id,message_type,sent_at,is_from_me)
SELECT ?,?, 'budget-decoy-' || (ones.n + 10*tens.n + 100*hundreds.n + 1000*thousands.n + 1), 'meeting_transcript', ?, TRUE
FROM digits ones CROSS JOIN digits tens CROSS JOIN digits hundreds CROSS JOIN digits thousands`), f.ConvID, f.Source.ID, q.Now.Add(-time.Hour))
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,occurrence_key,decision,created_at)
SELECT id,'unrelated@example.org','','link',? FROM messages WHERE source_id=? AND source_message_id LIKE 'budget-decoy-%'
AND CAST(substr(source_message_id,14) AS INTEGER) <= 5000`), q.Now, f.Source.ID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_key(message_id,kind,value,calendar_id)
SELECT id,'ical_uid','unrelated@example.org','' FROM messages WHERE source_id=? AND source_message_id LIKE 'budget-decoy-%'
AND CAST(substr(source_message_id,14) AS INTEGER) > 5000`), f.Source.ID)
	requirements.NoError(err)

	got, truncated, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 2)
	requirements.NoError(err)
	assertions.False(truncated, "returned transcript IDs: %v", got)
	assertions.Equal([]int64{wanted, aliased}, got)
}

func TestCadenceForPersonUsesSuppliedLastContact(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	// The existing attribute writer validates and stores a real integer frequency.
	days := int64(21)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		days int
		want string
	}{{30, "overdue"}, {10, "ok"}} {
		last := now.AddDate(0, 0, -tc.days)
		status, due, err := f.Store.CadenceForPersonContext(t.Context(), person.ID, &last, now)
		requirements.NoError(err)
		assertions.Equal(tc.want, status)
		requirements.NotNil(due)
		assertions.Equal(last.AddDate(0, 0, 21), *due)
	}
	status, due, err := f.Store.CadenceForPersonContext(t.Context(), person.ID, nil, now)
	requirements.NoError(err)
	assertions.Equal("unknown", status)
	assertions.Nil(due)
}

func TestCadenceForPersonReusesReadSnapshot(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineActivityFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	days := int64(21)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
	requirements.NoError(err)

	f.Store.DB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	snapshotCtx, release, err := f.Store.BeginReadSnapshotContext(ctx)
	requirements.NoError(err)
	defer release()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	last := now.AddDate(0, 0, -30)
	status, due, err := f.Store.CadenceForPersonContext(snapshotCtx, person.ID, &last, now)
	requirements.NoError(err)
	requirements.Equal("overdue", status)
	requirements.NotNil(due)
	requirements.Equal(last.AddDate(0, 0, 21), *due)
}
