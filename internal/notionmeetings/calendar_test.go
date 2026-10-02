package notionmeetings

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

const calendarPageID = "12345678-1234-1234-1234-123456789abc"

func joinMeeting(t *testing.T) *HydratedMeeting {
	t.Helper()
	h, err := NewHydrator(completeHydrationSource()).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(t, err)
	h.Discovery.Parent.PageID = calendarPageID
	return h
}

func joinEvent() CalendarEvent {
	return CalendarEvent{MessageID: 11, SourceID: 12, CalendarID: "primary", Event: gcal.Event{
		ID: "event-1", Summary: "Weekly planning", Start: gcal.EventDateTime{DateTime: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)}, End: gcal.EventDateTime{DateTime: time.Date(2026, 8, 29, 10, 30, 0, 0, time.UTC)},
		Attendees: []gcal.Attendee{{Email: "attendee@example.com", DisplayName: "Calendar name"}, {Email: "external@example.com", DisplayName: "External Invitee"}, {Email: "room@example.com", Resource: true}},
	}}
}

func TestCalendarJoinSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CalendarEvent)
		basis  string
	}{
		{"description", func(e *CalendarEvent) {
			e.Event.Description = `<a href="https://www.notion.so/Weekly-12345678123412341234123456789abc?pvs=4">notes</a>`
		}, "notion_link"},
		{"location", func(e *CalendarEvent) {
			e.Event.Location = "https://team.notion.site/12345678-1234-1234-1234-123456789abc"
		}, "notion_link"},
		{"conference", func(e *CalendarEvent) {
			e.Event.Raw = []byte(`{"conferenceData":{"notes":"https://notion.so/12345678123412341234123456789abc"}}`)
		}, "notion_link"},
		{"attachment", func(e *CalendarEvent) {
			e.Event.Raw = []byte(`{"attachments":[{"fileUrl":"https://notion.site/12345678123412341234123456789abc"}]}`)
		}, "notion_link"},
		{"source", func(e *CalendarEvent) {
			e.Event.Raw = []byte(`{"source":{"url":"https://notion.so/12345678123412341234123456789abc"}}`)
		}, "notion_link"},
		{"calendar notes", func(e *CalendarEvent) {
			e.Event.Description = "https://calendar.notion.so/meeting-notes?notionPageId=12345678-1234-1234-1234-123456789abc"
		}, "notion_link"},
		{"heuristic", func(e *CalendarEvent) {}, "heuristic"},
		{"other page blocks heuristic", func(e *CalendarEvent) { e.Event.Description = "https://notion.so/ffffffffffffffffffffffffffffffff" }, ""},
		{"wrong host", func(e *CalendarEvent) {
			e.Event.Description = "https://notion.so.evil.example/12345678123412341234123456789abc"
			e.Event.Summary = "Unrelated"
		}, ""},
		{"unrelated", func(e *CalendarEvent) { e.Event.Summary = "Unrelated" }, ""},
		{"cancelled", func(e *CalendarEvent) { e.Event.Status = gcal.StatusCancelled }, ""},
		{"all day", func(e *CalendarEvent) { e.Event.Start = gcal.EventDateTime{Date: "2026-08-29"} }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			h := joinMeeting(t)
			e := joinEvent()
			tc.change(&e)
			joinCalendar(h, []CalendarEvent{e})
			if tc.basis == "" {
				assert.Nil(h.CalendarMatch)
				require.Len(h.Attendees, 1)
				return
			}
			require.NotNil(h.CalendarMatch)
			assert.Equal(tc.basis, h.CalendarMatch.Basis)
			assert.Greater(h.CalendarMatch.Confidence, 0.0)
			require.Len(h.Attendees, 2)
			assert.Equal("attendee@example.com", h.Attendees[0].Email)
			assert.Equal(userAnchor("user-1"), h.Attendees[0].Anchor)
			assert.Equal("external@example.com", h.Attendees[1].Email)
			assert.Empty(h.Attendees[1].Anchor, "calendar emails cannot invent a Notion user identity")
			assert.Equal([]string{"user-2"}, h.UnresolvedAttendeeIDs)
			snapshot, err := h.ArchiveSnapshot(1, "work", "owner@example.com")
			require.NoError(err)
			assert.Contains(string(snapshot.Raw), `"calendar_match"`)
			assert.Contains(string(snapshot.Metadata), `"basis":"`+tc.basis+`"`)
		})
	}
}

func TestCalendarJoinRetrievedParentPageLink(t *testing.T) {
	h := joinMeeting(t)
	h.Discovery.Parent.PageID = "ffffffffffffffffffffffffffffffff"
	h.MeetingBlock.Parent.PageID = calendarPageID
	event := joinEvent()
	event.Event.Description = "https://notion.so/12345678123412341234123456789abc"
	event.Event.Summary = "Different title"
	event.Event.Start.DateTime = event.Event.Start.DateTime.Add(time.Hour)
	joinCalendar(h, []CalendarEvent{event})
	require.NotNil(t, h.CalendarMatch)
	assert.Equal(t, "notion_link", h.CalendarMatch.Basis)
	assert.Equal(t, normalizedPageID(calendarPageID), h.CalendarMatch.NotionPageID)
}

func TestCalendarJoinRepeatedTitleTokensDoNotCreateHeuristicMatch(t *testing.T) {
	h := joinMeeting(t)
	h.Discovery.MeetingNotes.Title = []RichText{{PlainText: "planning planning planning planning launch"}}
	event := joinEvent()
	event.Event.Summary = "planning unrelated discussion"
	event.Event.End.DateTime = event.Event.End.DateTime.Add(30 * time.Minute)
	joinCalendar(h, []CalendarEvent{event})
	assert.Nil(t, h.CalendarMatch)
	require.Len(t, h.Attendees, 1)
}

func TestCalendarJoinExactWinsAndAmbiguityIsDisplayOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	exact := joinEvent()
	exact.Event.ID = "exact"
	exact.Event.Description = "https://notion.so/12345678123412341234123456789abc"
	exact.Event.Summary = "Different title"
	exact.Event.Start.DateTime = exact.Event.Start.DateTime.Add(time.Hour)
	h := joinMeeting(t)
	joinCalendar(h, []CalendarEvent{joinEvent(), exact})
	require.NotNil(h.CalendarMatch)
	assert.Equal("exact", h.CalendarMatch.EventID)
	assert.Equal("notion_link", h.CalendarMatch.Basis)
	for _, linked := range []bool{false, true} {
		h := joinMeeting(t)
		a := joinEvent()
		if linked {
			a = exact
		}
		b := a
		b.Event.ID = "other"
		b.MessageID = 22
		joinCalendar(h, []CalendarEvent{a, b})
		assert.Nil(h.CalendarMatch)
		require.Len(h.Attendees, 1)
		assert.Contains(h.Warnings, "Google Calendar join was ambiguous; invitees remain unresolved")
	}
}

type fakeCalendarSource struct {
	events []CalendarEvent
	calls  int
	err    error
}

func (f *fakeCalendarSource) CalendarEvents(context.Context, string) ([]CalendarEvent, error) {
	f.calls++
	return f.events, f.err
}

func TestImporterCalendarJoinCachesAndLinksRealPeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	participant, err := st.EnsureParticipant("external@example.com", "External Invitee", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(err)
	source.usersErr = ErrUserInformation
	calendar := &fakeCalendarSource{events: []CalendarEvent{joinEvent()}}
	imp.WithCalendarSource(calendar)
	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE source_id=? AND source_message_id=?"), summary.SourceID, "meeting-1").Scan(&messageID))
	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(err)
	require.Len(recipients, 2)
	var linkedID int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT person_id FROM person_participants WHERE participant_id=?"), participant).Scan(&linkedID))
	assert.Equal(person.ID, linkedID)
	var meetingsForPerson int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients mr JOIN person_participants pp ON pp.participant_id=mr.participant_id WHERE mr.message_id=? AND pp.person_id=?`), messageID, person.ID).Scan(&meetingsForPerson))
	assert.Equal(1, meetingsForPerson)
	assert.Equal(1, calendar.calls)
}

func TestImporterJoinsSyncedCalendarArchiveAndScopesAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	meeting := hydrationMeeting()
	meeting.Parent.PageID = calendarPageID
	source.query.Results = []MeetingNote{meeting}
	source.usersErr = ErrUserInformation
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	ev := joinEvent().Event
	ev.Description = "https://notion.so/12345678123412341234123456789abc"
	heuristic := joinEvent().Event
	heuristic.ID = "event-heuristic"
	mock.FullEvents["primary"] = [][]gcal.Event{{ev, heuristic}}
	mock.FullSyncToken["primary"] = "sync-1"
	syncer := calsync.New(mock, st, calsync.Options{AccountEmail: "owner@example.com"})
	_, err := syncer.Full(t.Context())
	require.NoError(err)
	calendar, err := st.CalendarJoinEventsContext(t.Context(), " OWNER@EXAMPLE.COM ")
	require.NoError(err)
	require.Len(calendar, 2)
	var exactMessageID int64
	for _, candidate := range calendar {
		var event gcal.Event
		require.NoError(json.Unmarshal(candidate.Raw, &event))
		if event.ID == "event-1" {
			exactMessageID = candidate.MessageID
		}
	}
	require.NotZero(exactMessageID)
	wrong, err := st.CalendarJoinEventsContext(t.Context(), "other@example.com")
	require.NoError(err)
	assert.Empty(wrong)
	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true})
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE source_id=? AND source_message_id=?"), summary.SourceID, "meeting-1").Scan(&messageID))
	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(err)
	require.Len(recipients, 2)
	raw, err := st.GetMessageRawContext(t.Context(), messageID)
	require.NoError(err)
	assert.Contains(string(raw), `"basis":"notion_link"`)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Contains(content.SourceParticipants, meetingcontent.Participant{Name: "External Invitee", Email: "external@example.com", Role: "to"})
	again, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true})
	require.NoError(err)
	assert.Zero(again.MeetingsAdded)
	_, err = st.DB().Exec(st.Rebind("DELETE FROM message_raw WHERE message_id = ?"), exactMessageID)
	require.NoError(err)
	missingRaw, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true})
	require.NoError(err)
	assert.Equal(int64(1), missingRaw.MeetingsUpdated)
	raw, err = st.GetMessageRawContext(t.Context(), messageID)
	require.NoError(err)
	var evidence rawEvidence
	require.NoError(json.Unmarshal(raw, &evidence))
	require.NotNil(evidence.CalendarMatch)
	assert.Equal("event-1", evidence.CalendarMatch.EventID)
	assert.Equal("notion_link", evidence.CalendarMatch.Basis)
	assert.Contains(string(raw), "some Google Calendar join evidence was unavailable")
	recipients, err = st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(err)
	require.Len(recipients, 2)
	assert.ElementsMatch([]string{"attendee@example.com", "external@example.com"}, []string{
		recipients[0].EmailAddress, recipients[1].EmailAddress,
	})
	require.NoError(st.MarkMessageDeleted(calendar[0].SourceID, "event-1"))
	require.NoError(st.MarkMessageDeleted(calendar[0].SourceID, "event-heuristic"))
	deleted, err := st.CalendarJoinEventsContext(t.Context(), "owner@example.com")
	require.NoError(err)
	assert.Empty(deleted)
}

func TestCalendarJoinIgnoresCancellationWithRetainedRawEvent(t *testing.T) {
	require := require.New(t)
	st, _, _ := newImporterFixture(t)
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	ev := joinEvent().Event
	mock.FullEvents["primary"] = [][]gcal.Event{{ev}}
	mock.FullSyncToken["primary"] = "initial"
	syncer := calsync.New(mock, st, calsync.Options{AccountEmail: "owner@example.com"})
	_, err := syncer.Full(t.Context())
	require.NoError(err)
	mock.IncEvents["initial"] = [][]gcal.Event{{{ID: ev.ID, Status: gcal.StatusCancelled}}}
	mock.IncNextToken["initial"] = "cancelled"
	_, err = syncer.Incremental(t.Context())
	require.NoError(err)
	events, err := st.CalendarJoinEventsContext(t.Context(), "owner@example.com")
	require.NoError(err)
	assert.Empty(t, events, "Calendar cancellation updates metadata while retaining original raw JSON")
}

func TestCalendarJoinDeduplicatesSameEventOnMultipleCalendars(t *testing.T) {
	h := joinMeeting(t)
	a := joinEvent()
	a.Event.Description = "https://notion.so/12345678123412341234123456789abc"
	b := a
	b.MessageID = 22
	b.SourceID = 23
	b.CalendarID = "shared"
	joinCalendar(h, []CalendarEvent{a, b})
	require.NotNil(t, h.CalendarMatch)
	assert.Equal(t, "notion_link", h.CalendarMatch.Basis)
	require.Len(t, h.Attendees, 2)
}

func TestCalendarEvidenceFailureDoesNotBlockMeetingContent(t *testing.T) {
	_, _, imp := newImporterFixture(t)
	imp.WithCalendarSource(&fakeCalendarSource{err: errors.New("unreadable calendar evidence")})
	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true})
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.MeetingsProcessed)
	assert.Equal(t, int64(1), summary.MeetingsAdded)
}

func TestImporterPreservesArchivedCalendarJoinWhenEvidenceIsIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp := newImporterFixture(t)
	calendar := &fakeCalendarSource{events: []CalendarEvent{joinEvent()}}
	imp.WithCalendarSource(calendar)
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true}

	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	calendar.events = nil
	calendar.err = errors.New("unreadable calendar evidence")
	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)

	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		"SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?",
	), first.SourceID, "meeting-1").Scan(&messageID))
	raw, err := st.GetMessageRawContext(t.Context(), messageID)
	require.NoError(err)
	var evidence rawEvidence
	require.NoError(json.Unmarshal(raw, &evidence))
	require.NotNil(evidence.CalendarMatch)
	assert.Equal("event-1", evidence.CalendarMatch.EventID)
	assert.Equal("heuristic", evidence.CalendarMatch.Basis)
	assert.Len(evidence.CalendarMatch.Attendees, 2)

	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(err)
	require.Len(recipients, 2)
	assert.ElementsMatch([]string{"attendee@example.com", "external@example.com"}, []string{
		recipients[0].EmailAddress, recipients[1].EmailAddress,
	})
}

func TestCalendarEvidenceSkipsCorruptEventAndKeepsHealthyJoin(t *testing.T) {
	for _, failure := range []string{"json", "missing ID", "compression"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			meeting := hydrationMeeting()
			meeting.Parent.PageID = calendarPageID
			source.query.Results = []MeetingNote{meeting}
			mock := gcal.NewMockAPI()
			mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
			healthy := joinEvent().Event
			healthy.Description = "https://notion.so/12345678123412341234123456789abc"
			bad := healthy
			bad.ID = "broken-event"
			mock.FullEvents["primary"] = [][]gcal.Event{{bad, healthy}}
			mock.FullSyncToken["primary"] = "initial"
			_, err := calsync.New(mock, st, calsync.Options{AccountEmail: "owner@example.com"}).Full(t.Context())
			require.NoError(err)
			var id int64
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE source_message_id = ?"), "broken-event").Scan(&id))
			switch failure {
			case "json":
				require.NoError(st.UpsertMessageRawWithFormat(id, []byte("{"), gcal.RawFormat))
			case "missing ID":
				require.NoError(st.UpsertMessageRawWithFormat(id, []byte("{}"), gcal.RawFormat))
			case "compression":
				_, err = st.DB().Exec(st.Rebind("UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?"), []byte("invalid compressed data"), id)
				require.NoError(err)
			}
			events, err := (archiveCalendarSource{st}).CalendarEvents(t.Context(), "owner@example.com")
			require.Error(err)
			require.Len(events, 1)
			assert.Equal(healthy.ID, events[0].Event.ID)
			summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true})
			require.NoError(err)
			assert.Equal(int64(1), summary.MeetingsAdded)
			var meetingID int64
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?"), summary.SourceID, "meeting-1").Scan(&meetingID))
			raw, err := st.GetMessageRaw(meetingID)
			require.NoError(err)
			assert.Contains(string(raw), `"basis":"notion_link"`)
			assert.Contains(string(raw), "some Google Calendar join evidence was unavailable")
		})
	}
}
