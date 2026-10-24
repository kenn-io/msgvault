package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/search"
)

func calendarDocument(method, props string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:" + method + "\r\nBEGIN:VEVENT\r\nUID:family@example.com\r\n" + props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
}
func calendarFamilyDocument() []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:family@example.com\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Original series\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:family@example.com\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Moved occurrence\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
}
func icsSource(t *testing.T, st *Store) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("ics", "synthetic-calendar")
	require.NoError(t, err)
	return source.ID
}

func TestICalendarSeriesCancellationActivityEligibility(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := strings.Replace(string(calendarFamilyDocument()), "RECURRENCE-ID:", "SEQUENCE:3\r\nRECURRENCE-ID:", 1)
	imported, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
	requirements.NoError(err)
	requirements.Len(imported.MessageIDs, 2)
	// Drain initial work so a change to the master must rediscover the exception.
	_, err = st.db.Exec(`UPDATE activity_projection_queue SET processed_revision=revision`)
	requirements.NoError(err)
	for _, tc := range []struct {
		name, method, props string
		eligible            bool
	}{
		{"cancel", "CANCEL", "SEQUENCE:1\r\nSTATUS:CANCELLED", false},
		{"restore", "REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T120000Z\r\nRRULE:FREQ=DAILY;COUNT=2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument(tc.method, tc.props))
			requirements.NoError(err)
			candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), imported.MessageIDs[1:])
			requirements.NoError(err)
			requirements.Len(candidates, 1)
			assertions.Equal(tc.eligible, candidates[0].Eligible)
			queued, err := st.LoadQueuedActivityCandidatesContext(t.Context(), 100)
			requirements.NoError(err)
			var found bool
			for _, candidate := range queued {
				if candidate.MessageID == imported.MessageIDs[1] {
					found = true
					assertions.Equal(tc.eligible, candidate.Eligible)
				}
			}
			assertions.True(found, "the drained exception must be queued after its master changes")
			_, err = st.db.Exec(`UPDATE activity_projection_queue SET processed_revision=revision`)
			requirements.NoError(err)
		})
	}
}

func TestICalendarPersistenceRevisionsAndRebuild(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nSUMMARY:First\r\nRRULE:FREQ=DAILY;COUNT=2")
	first, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(first.MessageIDs, 1)
	id := first.MessageIDs[0]
	assertions.Equal(1, first.Changed)
	duplicate, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	assertions.Equal(1, duplicate.Unchanged)
	assertions.Equal(first.MessageIDs, duplicate.MessageIDs)
	older := strings.Replace(string(raw), "SEQUENCE:2", "SEQUENCE:1", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(older))
	requirements.NoError(err)
	stored, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	later := strings.Replace(string(raw), "20270301T120000Z", "20270301T130000Z", 1)
	later = strings.Replace(later, "20270302T120000Z", "20270302T140000Z", 1)
	later = strings.Replace(later, "20270302T130000Z", "20270302T150000Z", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	assertions.Equal("2027-03-02T14:00:00Z", calendarRows(t, st, id)[0].Event.Start.Format(time.RFC3339))

	stored, err = st.GetMessageRaw(id)
	requirements.NoError(err)
	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTAMP:20270301T130000Z")
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	row := calendarRows(t, st, id)[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("series", row.Event.Kind)
	assertions.NotNil(row.Event.Start)
	_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, id)
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, stored, "ics"))
	assertions.Equal(row.Event, calendarRows(t, st, id)[0].Event)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, id)[0].Event.Status)
	restore := strings.Replace(string(raw), "SEQUENCE:2", "SEQUENCE:4", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(restore))
	requirements.NoError(err)
	assertions.Equal("confirmed", calendarRows(t, st, id)[0].Event.Status)
}

func TestICalendarFTSIndexesInsertionAndRevision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	firstRaw := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:IndexTitleFirst\r\nDESCRIPTION:IndexBodyFirst")
	first, err := st.PersistICalendarContext(t.Context(), source, firstRaw)
	requirements.NoError(err)
	requirements.Len(first.MessageIDs, 1)
	firstMatches, _, err := st.SearchMessages("IndexBodyFirst", 0, 10)
	requirements.NoError(err)
	requirements.Len(firstMatches, 1)
	assertions.Equal(first.MessageIDs[0], firstMatches[0].ID)

	secondRaw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T130000Z\r\nSUMMARY:IndexTitleSecond\r\nDESCRIPTION:IndexBodySecond")
	_, err = st.PersistICalendarContext(t.Context(), source, secondRaw)
	requirements.NoError(err)
	secondMatches, _, err := st.SearchMessages("IndexBodySecond", 0, 10)
	requirements.NoError(err)
	requirements.Len(secondMatches, 1)
	assertions.Equal(first.MessageIDs[0], secondMatches[0].ID)
	staleMatches, _, err := st.SearchMessages("IndexBodyFirst", 0, 10)
	requirements.NoError(err)
	assertions.Empty(staleMatches)
}

func TestICalendarParticipantsAppearInAddressSearches(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Participant search\r\nORGANIZER;CN=Calendar Organizer:mailto:organizer@example.test\r\nATTENDEE;CN=Calendar Guest:mailto:guest@example.test")
	imported, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(imported.MessageIDs, 1)
	id := imported.MessageIDs[0]

	fromMatches, _, err := st.SearchMessagesQuery(search.Parse("from:organizer@example.test"), 0, 10)
	requirements.NoError(err)
	if assertions.Len(fromMatches, 1) {
		assertions.Equal(id, fromMatches[0].ID)
	}
	toMatches, _, err := st.SearchMessagesQuery(search.Parse("to:guest@example.test"), 0, 10)
	requirements.NoError(err)
	if assertions.Len(toMatches, 1) {
		assertions.Equal(id, toMatches[0].ID)
	}
	organizerTextMatches, _, err := st.SearchMessagesQuery(search.Parse("organizer"), 0, 10)
	requirements.NoError(err)
	assertions.Len(organizerTextMatches, 1)
	guestTextMatches, _, err := st.SearchMessagesQuery(search.Parse("guest"), 0, 10)
	requirements.NoError(err)
	assertions.Len(guestTextMatches, 1)
	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.Len(candidates[0].Counterparts, 2)

	var senderEmail, senderName string
	requirements.NoError(st.db.QueryRow(`
		SELECT p.email_address,p.display_name
		FROM messages m JOIN participants p ON p.id=m.sender_id WHERE m.id=?`, id,
	).Scan(&senderEmail, &senderName))
	assertions.Equal("organizer@example.test", senderEmail)
	assertions.Equal("Calendar Organizer", senderName)
	var recipientEmail, recipientName string
	requirements.NoError(st.db.QueryRow(`
		SELECT p.email_address,mr.display_name
		FROM message_recipients mr JOIN participants p ON p.id=mr.participant_id
		WHERE mr.message_id=? AND mr.recipient_type='to'`, id,
	).Scan(&recipientEmail, &recipientName))
	assertions.Equal("guest@example.test", recipientEmail)
	assertions.Equal("Calendar Guest", recipientName)
}

func TestICalendarActivityUsesStatusAndCivilDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.db.Exec(`UPDATE sources SET sync_config=? WHERE id=?`, `{"time_zone":"Asia/Tokyo"}`, source)
	requirements.NoError(err)

	allDayRaw := strings.Replace(string(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART;VALUE=DATE:20270314\r\nDTEND;VALUE=DATE:20270315")),
		"UID:family@example.com", "UID:all-day@example.test", 1)
	allDay, err := st.PersistICalendarContext(t.Context(), source, []byte(allDayRaw))
	requirements.NoError(err)
	requirements.Len(allDay.MessageIDs, 1)
	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), allDay.MessageIDs)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.True(candidates[0].Eligible)
	assertions.True(candidates[0].DateOnly)
	requirements.NotNil(candidates[0].SentAt)
	assertions.Equal(time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC), candidates[0].SentAt.UTC())

	timedRaw := strings.Replace(string(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270305T120000Z\r\nDTEND:20270305T130000Z")),
		"UID:family@example.com", "UID:cancelled@example.test", 1)
	timed, err := st.PersistICalendarContext(t.Context(), source, []byte(timedRaw))
	requirements.NoError(err)
	cancelRaw := strings.Replace(timedRaw, "BEGIN:VEVENT", "BEGIN:VEVENT\r\nSTATUS:CANCELLED", 1)
	_, err = st.PersistICalendarContext(t.Context(), source,
		[]byte(strings.Replace(cancelRaw, "METHOD:REQUEST", "METHOD:CANCEL", 1)))
	requirements.NoError(err)
	candidates, err = st.LoadActivityCandidatesByIDContext(t.Context(), timed.MessageIDs)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.False(candidates[0].Eligible)
}

func TestICalendarOrphanAllDayCancellationKeepsCivilDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	imported, err := st.PersistICalendarContext(t.Context(), source,
		calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nRECURRENCE-ID;VALUE=DATE:20270314"))
	requirements.NoError(err)
	requirements.Len(imported.MessageIDs, 1)

	var sent sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, imported.MessageIDs[0]).Scan(&sent))
	requirements.True(sent.Valid)
	assertions.Equal(time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC), sent.Time.UTC())
	event := calendarRows(t, st, imported.MessageIDs[0])[0].Event
	assertions.Equal("cancelled", event.Status)
	assertions.Equal("2027-03-14", event.Original.Date)
}

func TestICalendarSparseCancellationPreservesMessageSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	request := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Preserved title\r\nDESCRIPTION:Preserved description")
	first, err := st.PersistICalendarContext(t.Context(), source, request)
	requirements.NoError(err)
	var before sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, first.MessageIDs[0]).Scan(&before))
	requirements.True(before.Valid)

	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTAMP:20270301T130000Z")
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	var after sql.NullTime
	var subject string
	requirements.NoError(st.db.QueryRow(`SELECT sent_at,subject FROM messages WHERE id=?`, first.MessageIDs[0]).Scan(&after, &subject))
	assertions.Equal(before, after)
	assertions.Equal("Preserved title", subject)
	var conversationTitle string
	requirements.NoError(st.db.QueryRow(`
		SELECT c.title FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?`,
		first.MessageIDs[0]).Scan(&conversationTitle))
	assertions.Equal("Preserved title", conversationTitle)
	body, err := st.GetMessageBodyText(first.MessageIDs[0])
	requirements.NoError(err)
	assertions.Equal("Preserved description", body)
	assertions.NotNil(calendarRows(t, st, first.MessageIDs[0])[0].Event.Start)
}

func TestICalendarActiveRevisionCanClearSummary(t *testing.T) {
	for _, tt := range []struct {
		name, summary string
		series        bool
	}{
		{name: "removed standalone"},
		{name: "empty standalone", summary: "SUMMARY:"},
		{name: "removed series master", series: true},
		{name: "empty series master", summary: "SUMMARY:", series: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			firstProps := "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:StaleActiveRevisionTitle"
			if tt.series {
				firstProps += "\r\nRRULE:FREQ=DAILY;COUNT=2"
			}
			first, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", firstProps))
			requirements.NoError(err)
			requirements.Len(first.MessageIDs, 1)

			props := "SEQUENCE:2\r\nDTSTART:20270302T130000Z"
			if tt.series {
				props += "\r\nRRULE:FREQ=DAILY;COUNT=2"
			}
			if tt.summary != "" {
				props += "\r\n" + tt.summary
			}
			_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", props))
			requirements.NoError(err)

			var subject sql.NullString
			requirements.NoError(st.db.QueryRow(`SELECT subject FROM messages WHERE id=?`, first.MessageIDs[0]).Scan(&subject))
			assertions.False(subject.Valid)
			assertions.Empty(subject.String)
			var conversationTitle sql.NullString
			requirements.NoError(st.db.QueryRow(`
				SELECT c.title FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?`,
				first.MessageIDs[0]).Scan(&conversationTitle))
			assertions.False(conversationTitle.Valid)
			assertions.Empty(conversationTitle.String)
			matches, _, err := st.SearchMessages("StaleActiveRevisionTitle", 0, 10)
			requirements.NoError(err)
			assertions.Empty(matches, "the previous title should be removed from FTS")
		})
	}
}

func TestICalendarDetachedExceptionKeepsSeriesConversationTitle(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	master, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Series title"))
	requirements.NoError(err)
	exception, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Moved occurrence title"))
	requirements.NoError(err)
	var title string
	requirements.NoError(st.db.QueryRow(`SELECT c.title FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?`, master.MessageIDs[0]).Scan(&title))
	assertions.Equal("Series title", title)
	var exceptionTitle string
	requirements.NoError(st.db.QueryRow(`SELECT subject FROM messages WHERE id=?`, exception.MessageIDs[0]).Scan(&exceptionTitle))
	assertions.Equal("Moved occurrence title", exceptionTitle)
}

func TestICalendarMovedExceptionCancellationKeepsEffectiveSentAt(t *testing.T) {
	for _, tc := range []struct {
		name, props, recurrence string
		allDay                  bool
	}{
		{"timed", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDTEND:20270303T150000Z", "RECURRENCE-ID:20270303T120000Z", false},
		{"all day", "RECURRENCE-ID;VALUE=DATE:20270303\r\nDTSTART;VALUE=DATE:20270305\r\nDTEND;VALUE=DATE:20270306", "RECURRENCE-ID;VALUE=DATE:20270303", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			moved, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\n"+tc.props))
			requirements.NoError(err)
			var before sql.NullTime
			requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, moved.MessageIDs[0]).Scan(&before))
			requirements.True(before.Valid)

			cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\n"+tc.recurrence)
			_, err = st.PersistICalendarContext(t.Context(), source, cancel)
			requirements.NoError(err)
			var after sql.NullTime
			requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, moved.MessageIDs[0]).Scan(&after))
			assertions.Equal(before, after)
			event := calendarRows(t, st, moved.MessageIDs[0])[0].Event
			if tc.allDay {
				assertions.Equal(time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC), before.Time.UTC())
				assertions.True(event.AllDay)
				assertions.Equal("2027-03-05", event.StartDate)
			} else {
				requirements.NotNil(event.Start)
				assertions.Equal(before.Time.UTC(), event.Start.UTC())
			}
		})
	}
}

func TestICalendarImportAtomicAndScoped(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z")
	second := strings.Replace(string(raw), "family@example.com", "second@example.org", 1)
	multi := []byte(strings.Replace(string(raw), "END:VCALENDAR\r\n", strings.TrimPrefix(strings.TrimSuffix(second, "END:VCALENDAR\r\n"), "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n")+"END:VCALENDAR\r\n", 1))
	sum, err := st.PersistICalendarContext(t.Context(), source, multi)
	requirements.NoError(err)
	requirements.Len(sum.MessageIDs, 2)
	for _, id := range sum.MessageIDs {
		stored, err := st.GetMessageRaw(id)
		requirements.NoError(err)
		assertions.Contains(string(stored), "BEGIN:VEVENT")
		assertions.Equal(1, strings.Count(string(stored), "BEGIN:VEVENT"))
	}
	invalid := strings.Replace(string(multi), "UID:second@example.org", "UID:", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(invalid))
	requirements.Error(err)
	statements := []string{
		`CREATE TRIGGER reject_calendar_uid BEFORE INSERT ON calendar_events WHEN NEW.provider_event_id='calendar-invite:60ca29ff47dc012b6a39e74d16ead805f3b977f6152e58444246d537098057ae' BEGIN SELECT RAISE(ABORT, 'calendar projection blocked'); END`,
	}
	if st.IsPostgreSQL() {
		statements = []string{
			`CREATE FUNCTION reject_calendar_uid() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.provider_event_id='calendar-invite:60ca29ff47dc012b6a39e74d16ead805f3b977f6152e58444246d537098057ae' THEN RAISE EXCEPTION 'calendar projection blocked'; END IF; RETURN NEW; END $$`,
			`CREATE TRIGGER reject_calendar_uid BEFORE INSERT ON calendar_events FOR EACH ROW EXECUTE FUNCTION reject_calendar_uid()`,
		}
	}
	for _, statement := range statements {
		_, err = st.db.Exec(statement)
		requirements.NoError(err)
	}
	atomicFailure := strings.Replace(string(multi), "UID:family@example.com", "UID:first-new@example.org", 1)
	atomicFailure = strings.Replace(atomicFailure, "UID:second@example.org", "UID:blocked@example.org", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(atomicFailure))
	requirements.Error(err)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.source_id=? AND e.provider_event_id IN (?,?)`, source, calevent.ICSMessageID("first-new@example.org", ""), calevent.ICSMessageID("blocked@example.org", "")).Scan(&count))
	assertions.Zero(count, "an earlier new component is rolled back when a later component fails")
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, source).Scan(&count))
	assertions.Equal(2, count)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.PersistICalendarContext(ctx, source, raw)
	requirements.ErrorIs(err, context.Canceled)
}
func TestICalendarFloatingIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"America/New_York"}`))
	master, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
	requirements.NoError(err)
	child := calendarDocument("REQUEST", "RECURRENCE-ID:20270303T090000\r\nDTSTART:20270303T100000\r\nDURATION:PT1H")
	first, err := st.PersistICalendarContext(t.Context(), source, child)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"UTC"}`))
	again, err := st.PersistICalendarContext(t.Context(), source, child)
	requirements.NoError(err)
	assertions.Equal(first.MessageIDs, again.MessageIDs)
	assertions.Equal("20270303T090000Z", calendarRows(t, st, first.MessageIDs[0])[0].Event.Original.Key())
	assertions.Equal("series", calendarRows(t, st, master.MessageIDs[0])[0].Event.Kind)
}
func TestICalendarConcurrentRevisions(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, seq := range []int{1, 2} {
		wg.Go(func() {
			_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", fmt.Sprintf("SEQUENCE:%d\r\nDTSTART:20270302T120000Z", seq)))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		requirements.NoError(err)
	}
	var id int64
	requirements.NoError(st.db.QueryRow(`SELECT id FROM messages WHERE source_id=?`, source).Scan(&id))
	metadata, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	var meta calevent.ICSMetadata
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &meta))
	assertions.Equal(int64(2), meta.Sequence)
}

func TestICalendarRawIsNotEmailDedupEvidence(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	first := string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	event := "BEGIN:VEVENT\r\nUID:other@example.com\r\nDTSTART:20270303T120000Z\r\nEND:VEVENT\r\n"
	raw := []byte(strings.Replace(first, "END:VCALENDAR\r\n", event+"END:VCALENDAR\r\n", 1))
	sum, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(sum.MessageIDs, 2)
	candidates, err := st.GetAllRawMIMECandidates(source)
	requirements.NoError(err)
	assert.Empty(t, candidates, "the shared VCALENDAR bytes do not prove the VEVENT messages are duplicates")
}

func TestICalendarUnchangedUnsupportedSeries(t *testing.T) {
	for _, rule := range []string{"RRULE:FREQ=HOURLY;COUNT=2", "EXRULE:FREQ=DAILY;COUNT=1"} {
		t.Run(rule, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\n"+rule)
			first, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			assertions.Equal(1, first.Changed)
			assertions.Equal(1, first.Unexpandable)
			again, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			assertions.Zero(again.Changed)
			assertions.Equal(1, again.Unchanged)
			assertions.Equal(1, again.Unexpandable)
		})
	}
}

func TestICalendarRecurrenceErrorsFromRaw(t *testing.T) {
	for _, props := range []string{
		"DTSTART:20270313T090000Z\r\nRRULE:FREQ=HOURLY;COUNT=2",
		"DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D\r\nRRULE:FREQ=DAILY;COUNT=2",
	} {
		t.Run(props, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			raw := calendarDocument("REQUEST", props)
			first, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			requirements.Len(first.MessageIDs, 1)
			// Keep only the accepted revision. Recurrence diagnostics must follow
			// the retained event, independently of derived metadata.
			metadata, err := json.Marshal(calevent.ICSMetadata{Version: 1})
			requirements.NoError(err)
			_, err = st.db.Exec(`UPDATE messages SET metadata=? WHERE id=?`, string(metadata), first.MessageIDs[0])
			requirements.NoError(err)
			again, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			assertions.Zero(again.Changed)
			assertions.Equal(1, again.Unchanged)
			assertions.Equal(1, again.Unexpandable)
			from := time.Date(2027, 3, 13, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			requirements.NoError(err)
			requirements.Len(page.Unexpandable, 1)
			assertions.Equal(first.MessageIDs[0], page.Unexpandable[0].MessageID)
			assertions.Empty(page.Occurrences)
		})
	}
}

func TestICalendarCancelledExceptionOccurrences(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	family, err := st.PersistICalendarContext(t.Context(), source, calendarFamilyDocument())
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z"))
	requirements.NoError(err)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal("cancelled", page.Occurrences[1].Status)
	assertions.Equal("Moved occurrence", page.Occurrences[1].Subject)
	var count int
	var last time.Time
	requirements.NoError(st.db.QueryRow(`SELECT c.message_count,c.last_message_at FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE m.id=?`, family.MessageIDs[0]).Scan(&count, &last))
	assertions.Equal(2, count)
	assertions.Equal(time.Date(2027, 3, 3, 14, 0, 0, 0, time.UTC), last.UTC())
	t.Run("unedited occurrence inherits master", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		source := icsSource(t, st)
		_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Team meeting\r\nATTENDEE:mailto:guest@example.org\r\nORGANIZER:mailto:owner@example.com"))
		requirements.NoError(err)
		_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "STATUS:CANCELLED\r\nRECURRENCE-ID:20270303T120000Z"))
		requirements.NoError(err)
		from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err)
		requirements.Len(page.Occurrences, 1)
		assertions.Equal("Team meeting", page.Occurrences[0].Subject)
		assertions.Equal("cancelled", page.Occurrences[0].Status)
		assertions.Len(page.Occurrences[0].Attendees, 2)
	})
}

func TestICalendarZeroDurationAtWindowStart(t *testing.T) {
	for _, recurrence := range []string{""} {
		t.Run(recurrence, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z"+recurrence))
			requirements.NoError(err)
			from := time.Date(2027, 3, 2, 12, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(from, page.Occurrences[0].Start)
			assertions.Equal(from, page.Occurrences[0].End)
		})
	}
}

func TestICalendarFoldedUTF8AndReplyImport(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("PUBLISH", "DTSTART:20270302T120000Z\r\nSUMMARY:Caf\xc3\r\n \xa9")
	result, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(result.MessageIDs, 1)
	assertions.Equal("Café", calendarRows(t, st, result.MessageIDs...)[0].Subject)
	stored, err := st.GetMessageRaw(result.MessageIDs[0])
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	result, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REPLY", "ATTENDEE;PARTSTAT=DECLINED:mailto:guest@example.org"))
	requirements.NoError(err)
	assertions.Zero(result.Changed)
	assertions.Empty(result.MessageIDs)
}

func TestICalendarNominalDurationZoneRefresh(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "DTSTART:20270313T090000\r\nDURATION:P1D\r\nRRULE:FREQ=DAILY;COUNT=2")
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	from := time.Date(2027, 3, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		zone         string
		unexpandable int
	}{{"America/New_York", 1}, {"UTC", 0}} {
		requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"`+tc.zone+`"}`))
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 10))
		requirements.NoError(err)
		assertions.Len(page.Unexpandable, tc.unexpandable)
		result, err := st.PersistICalendarContext(t.Context(), source, raw)
		requirements.NoError(err)
		assertions.Equal(tc.unexpandable, result.Unexpandable)
	}
}

func TestICalendarSourceZoneRefreshesMessageTimestampAndConversationStats(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"UTC"}`))
	raw := calendarDocument("REQUEST", "DTSTART:20270310T090000\r\nDURATION:PT1H\r\nSUMMARY:Floating event")
	result, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(result.MessageIDs, 1)
	id := result.MessageIDs[0]
	var conversationID int64
	var sentAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT conversation_id,sent_at FROM messages WHERE id=?`, id).Scan(&conversationID, &sentAt))
	assertions.Equal(time.Date(2027, 3, 10, 9, 0, 0, 0, time.UTC), sentAt.UTC())
	var lastMessageAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT last_message_at FROM conversations WHERE id=?`, conversationID).Scan(&lastMessageAt))
	assertions.Equal(sentAt.UTC(), lastMessageAt.UTC())

	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"America/New_York"}`))
	want := time.Date(2027, 3, 10, 14, 0, 0, 0, time.UTC)
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, id).Scan(&sentAt))
	assertions.Equal(want, sentAt.UTC())
	requirements.NoError(st.db.QueryRow(`SELECT last_message_at FROM conversations WHERE id=?`, conversationID).Scan(&lastMessageAt))
	assertions.Equal(want, lastMessageAt.UTC())
	assertions.Equal(want.Format(time.RFC3339), calendarRows(t, st, id)[0].Event.Start.Format(time.RFC3339))

	// An unchanged import still repairs the message timestamp if an older
	// projection refresh updated typed bounds without refreshing sent_at.
	_, err = st.db.Exec(`UPDATE messages SET sent_at=? WHERE id=?`, time.Date(2027, 3, 10, 9, 0, 0, 0, time.UTC), id)
	requirements.NoError(err)
	unchanged, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	assertions.Equal(1, unchanged.Unchanged)
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, id).Scan(&sentAt))
	assertions.Equal(want, sentAt.UTC())
	requirements.NoError(st.db.QueryRow(`SELECT last_message_at FROM conversations WHERE id=?`, conversationID).Scan(&lastMessageAt))
	assertions.Equal(want, lastMessageAt.UTC())
}

func TestICalendarProjectionUpgradeAddsStartWallAndRebuilds(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"America/New_York"}`))
	raw := calendarDocument("REQUEST", "DTSTART;TZID=America/New_York:20270314T023000\r\nRRULE:FREQ=DAILY;COUNT=2")
	result, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(result.MessageIDs, 1)
	id := result.MessageIDs[0]

	// Reproduce an archive that has the v1 projection ledger but lacks the
	// stored wall DTSTART column and still needs its event row rebuilt.
	_, err = st.db.Exec(`ALTER TABLE calendar_events DROP COLUMN start_wall`)
	requirements.NoError(err)
	_, err = st.db.Exec(st.Rebind(`UPDATE calendar_events SET projection_version = 1 WHERE message_id = ?`), id)
	requirements.NoError(err)
	_, err = st.db.Exec(st.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'`), strconv.Itoa(SchemaVersion-1))
	requirements.NoError(err)
	_, err = st.db.Exec(st.Rebind(`INSERT INTO applied_migrations(name, version) VALUES (?, 1) ON CONFLICT(name) DO UPDATE SET version = excluded.version`), migrationCalendarEventProjectionV1)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v2'`)
	requirements.NoError(err)

	requirements.NoError(st.InitSchemaContext(t.Context()))
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	requirements.NotNil(rows[0].Event.StartWall)
	assertions.Equal("2027-03-14T02:30:00Z", rows[0].Event.StartWall.Format(time.RFC3339))
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal([]string{"2027-03-14T07:30:00Z", "2027-03-15T06:30:00Z"}, []string{
		page.Occurrences[0].Start.UTC().Format(time.RFC3339),
		page.Occurrences[1].Start.UTC().Format(time.RFC3339),
	})
	applied, err := st.IsMigrationAppliedContext(t.Context(), migrationCalendarEventProjectionV2, calendarEventProjectionVersion)
	requirements.NoError(err)
	assertions.True(applied)
}

func TestICalendarRecurrencePreservesWallTimeAcrossDSTGap(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"America/New_York"}`))
	raw := calendarDocument("REQUEST", "DTSTART;TZID=America/New_York:20270314T023000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3")
	result, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(result.MessageIDs, 1)
	id := result.MessageIDs[0]
	row := calendarRows(t, st, id)[0]
	requirements.NotNil(row.Event.StartWall)
	assertions.Equal("2027-03-14T02:30:00Z", row.Event.StartWall.Format(time.RFC3339))
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 3)
	assertions.Equal([]string{
		"2027-03-14T07:30:00Z",
		"2027-03-15T06:30:00Z",
		"2027-03-16T06:30:00Z",
	}, []string{
		page.Occurrences[0].Start.UTC().Format(time.RFC3339),
		page.Occurrences[1].Start.UTC().Format(time.RFC3339),
		page.Occurrences[2].Start.UTC().Format(time.RFC3339),
	})
}

func TestICalendarRejectUnknownMetadataVersion(t *testing.T) {
	for _, exception := range []bool{false, true} {
		t.Run(strconv.FormatBool(exception), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			first, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
			requirements.NoError(err)
			id := first.MessageIDs[0]
			requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"ics_version":2}`, Valid: true}))
			props := "SEQUENCE:1\r\nDTSTART:20270302T120000Z"
			if exception {
				props += "\r\nRECURRENCE-ID:20270302T120000Z"
			}
			_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", props))
			requirements.ErrorContains(err, "2")
			metadata, err := st.GetMessageMetadata(id)
			requirements.NoError(err)
			assertions.JSONEq(`{"ics_version":2}`, metadata.String)
		})
	}
}

func TestICalendarMasterSupersedesOmittedExceptions(t *testing.T) {
	for _, tc := range []struct {
		sequence int
		included bool
	}{{0, false}, {2, false}, {3, false}, {0, true}} {
		t.Run(fmt.Sprintf("sequence=%d/included=%t", tc.sequence, tc.included), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			sequence := tc.sequence
			st := newRFC822IDBackfillBackendStore(t)
			master := "ORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"
			_, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", master)))
			exception := fmt.Sprintf("ORGANIZER:mailto:organizer@example.com\r\nSEQUENCE:%d\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nDURATION:PT1H", sequence)
			persistInviteEmail(t, st, "exception", inviteEmail(calendarDocument("REQUEST", exception)))
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 2)
			raw, err := st.GetMessageRaw(ids[1])
			requirements.NoError(err)
			reschedule := calendarDocument("REQUEST", "SEQUENCE:2\r\n"+strings.Replace(master, "120000", "140000", 1))
			if tc.included {
				reschedule = []byte(strings.Replace(string(reschedule), "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:family@example.com\r\n"+exception+"\r\nEND:VEVENT\r\nEND:VCALENDAR", 1))
			}
			persistInviteEmail(t, st, "reschedule", inviteEmail(reschedule))
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			requirements.NoError(err)
			want := 3
			if sequence == 0 && !tc.included {
				want = 2
			}
			assertions.Len(page.Occurrences, want)
			metadata, err := st.GetMessageMetadata(ids[1])
			requirements.NoError(err)
			meta, err := calevent.ParseICSMetadata([]byte(metadata.String))
			requirements.NoError(err)
			assertions.Equal(sequence == 0 && !tc.included, meta.Superseded)
			assertions.Equal(int64(sequence), meta.Sequence)
			stored, err := st.GetMessageRaw(ids[1])
			requirements.NoError(err)
			assertions.Equal(raw, stored)
			_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id=?`, ids[1])
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageRawWithFormat(ids[1], raw, "ics"))
			page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			requirements.NoError(err)
			assertions.Len(page.Occurrences, want)
			if sequence == 0 {
				persistInviteEmail(t, st, "revival", inviteEmail(calendarDocument("REQUEST", strings.Replace(exception, "SEQUENCE:0", "SEQUENCE:3", 1))))
				page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
				requirements.NoError(err)
				assertions.Len(page.Occurrences, 3)
			}
		})
	}
}

func TestICalendarCorruptSeriesDegradesPerRow(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	bad, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nRRULE:FREQ=DAILY;COUNT=2"))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(strings.ReplaceAll(string(calendarDocument("REQUEST", "DTSTART:20270302T160000Z\r\nSUMMARY:Readable")), "family@example.com", "other@example.com")))
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression=NULL WHERE message_id=?`, []byte("invalid retained calendar"), bad.MessageIDs[0])
	requirements.NoError(err)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("Readable", page.Occurrences[0].Subject)
	requirements.Len(page.Unexpandable, 1)
	assertions.Equal(bad.MessageIDs[0], page.Unexpandable[0].MessageID)
	assertions.NotEmpty(page.Unexpandable[0].Reason)
}

func TestICalendarExceptionArrivalOrder(t *testing.T) {
	for _, masterFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(masterFirst), func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			master := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2")
			exception := calendarDocument("REQUEST", "SEQUENCE:0\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nDURATION:PT1H")
			items := [][]byte{exception, master}
			if masterFirst {
				items = [][]byte{master, exception}
			}
			for _, raw := range items {
				_, err := st.PersistICalendarContext(t.Context(), source, raw)
				require.NoError(t, err)
			}
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			require.NoError(t, err)
			assert.Len(t, page.Occurrences, 2)
		})
	}
}

func TestICalendarRecurrenceEvidenceBatched(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "google-series", strings.Replace(calendarRaw, `"id":`, `"recurrence":["RRULE:FREQ=DAILY;COUNT=2"],"id":`, 1))
	id, err := st.PersistMessageContext(t.Context(), data)
	requirements.NoError(err)
	google := calendarRows(t, st, id)
	requirements.Len(google, 1)
	requirements.Equal("series", google[0].Event.Kind)
	var queries int
	rebind := st.db.rebind
	st.db.rebind = func(query string) string {
		queries++
		return rebind(query)
	}
	remaining := calrecur.RequestReadBudget
	requirements.NoError(loadCalendarICSRecurrenceErrors(t.Context(), st.db, google, &remaining))
	assertions.Zero(queries, "Google series need no ICS evidence query")
	source := icsSource(t, st)
	var events []CalendarEventRow
	for _, uid := range []string{"one", "two", "bad"} {
		raw := []byte(strings.ReplaceAll(string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nRRULE:FREQ=DAILY;COUNT=2")), "family@example.com", uid+"@example.com"))
		sum, err := st.PersistICalendarContext(t.Context(), source, raw)
		requirements.NoError(err)
		events = append(events, calendarRows(t, st, sum.MessageIDs...)[0])
	}
	_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression=NULL WHERE message_id=?`, []byte("broken"), events[2].MessageID)
	requirements.NoError(err)
	queries = 0
	requirements.NoError(loadCalendarICSRecurrenceErrors(t.Context(), st.db, events, &remaining))
	assertions.Equal(1, queries)
	assertions.Empty(events[0].Event.RecurrenceError)
	assertions.Empty(events[1].Event.RecurrenceError)
	assertions.NotEmpty(events[2].Event.RecurrenceError)
}
