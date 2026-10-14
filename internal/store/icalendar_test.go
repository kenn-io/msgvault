package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
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

func removeICSActivityMetadataFields(t *testing.T, st *Store, messageID int64) {
	t.Helper()
	requirements := require.New(t)
	var raw string
	requirements.NoError(st.db.QueryRow(`SELECT metadata FROM messages WHERE id=?`, messageID).Scan(&raw))
	var metadata map[string]any
	requirements.NoError(json.Unmarshal([]byte(raw), &metadata))
	delete(metadata, "status")
	delete(metadata, "all_day")
	encoded, err := json.Marshal(metadata)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET metadata=? WHERE id=?`, string(encoded), messageID)
	requirements.NoError(err)
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
	cancel := calendarDocument("CANCEL", "SEQUENCE:3\r\nDTSTAMP:20270301T130000Z")
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	row := calendarRows(t, st, id)[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("series", row.Event.Kind)
	assertions.True(row.Event.HasBounds)
	_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, id)
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, cancel, "ics"))
	assertions.Equal(row.Event, calendarRows(t, st, id)[0].Event)
	_, err = st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, id)[0].Event.Status)
	restore := strings.Replace(string(raw), "SEQUENCE:2", "SEQUENCE:4", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(restore))
	requirements.NoError(err)
	assertions.Equal("confirmed", calendarRows(t, st, id)[0].Event.Status)
	assertions.Equal(fmt.Sprintf("evt:%d", id), FormatOccurrenceRef(OccurrenceRef{MessageID: id}))
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

	family, err := st.PersistICalendarContext(t.Context(), source, calendarFamilyDocument())
	requirements.NoError(err)
	requirements.Len(family.MessageIDs, 2)
	childID := family.MessageIDs[1]
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2"))
	requirements.NoError(err)
	candidates, err = st.LoadActivityCandidatesByIDContext(t.Context(), []int64{childID})
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.False(candidates[0].Eligible)

	timedRaw := strings.Replace(string(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270305T120000Z\r\nDTEND:20270305T130000Z")),
		"UID:family@example.com", "UID:cancelled@example.test", 1)
	timed, err := st.PersistICalendarContext(t.Context(), source, []byte(timedRaw))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source,
		[]byte(strings.Replace(timedRaw, "METHOD:REQUEST", "METHOD:CANCEL", 1)))
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
		calendarDocument("CANCEL", "SEQUENCE:2\r\nRECURRENCE-ID;VALUE=DATE:20270314"))
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

func TestICalendarActivityUsesTypedStateForLegacyMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	first, err := st.PersistICalendarContext(t.Context(), source,
		calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART;VALUE=DATE:20270314\r\nDTEND;VALUE=DATE:20270315"))
	requirements.NoError(err)
	requirements.Len(first.MessageIDs, 1)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2"))
	requirements.NoError(err)
	removeICSActivityMetadataFields(t, st, first.MessageIDs[0])

	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), first.MessageIDs)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.True(candidates[0].DateOnly)
	assertions.False(candidates[0].Eligible)
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

	cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTAMP:20270301T130000Z")
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
	assertions.True(calendarRows(t, st, first.MessageIDs[0])[0].Event.HasBounds)
}

func TestICalendarCancellationWithStartPreservesInheritedEnd(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	request := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z")
	first, err := st.PersistICalendarContext(t.Context(), source, request)
	requirements.NoError(err)

	cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTART:20270302T120000Z")
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)

	event := calendarRows(t, st, first.MessageIDs[0])[0].Event
	requirements.NotNil(event.Start)
	requirements.NotNil(event.End)
	assertions.Equal("20270302T120000Z", event.Start.UTC().Format("20060102T150405Z"))
	assertions.Equal("20270302T130000Z", event.End.UTC().Format("20060102T150405Z"))
}

func TestICalendarCancellationWithChangedStartPreservesConsistentBounds(t *testing.T) {
	for _, tt := range []struct {
		name, requestProps, cancelProps, wantStart, wantEnd string
	}{
		{
			name:         "timed",
			requestProps: "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z",
			cancelProps:  "SEQUENCE:2\r\nDTSTART:20270302T140000Z",
			wantStart:    "20270302T140000Z",
			wantEnd:      "20270302T150000Z",
		},
		{
			name:         "all day",
			requestProps: "SEQUENCE:1\r\nDTSTART;VALUE=DATE:20270302\r\nDTEND;VALUE=DATE:20270304",
			cancelProps:  "SEQUENCE:2\r\nDTSTART;VALUE=DATE:20270305",
			wantStart:    "2027-03-05",
			wantEnd:      "2027-03-07",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			first, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", tt.requestProps))
			requirements.NoError(err)
			requirements.Len(first.MessageIDs, 1)

			_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", tt.cancelProps))
			requirements.NoError(err)

			event := calendarRows(t, st, first.MessageIDs[0])[0].Event
			requirements.NotNil(event.Start)
			requirements.NotNil(event.End)
			assertions.Equal("cancelled", event.Status)
			assertions.True(event.HasBounds)
			assertions.False(event.End.Before(*event.Start))
			if event.AllDay {
				assertions.Equal(tt.wantStart, event.StartDate)
				assertions.Equal(tt.wantEnd, event.EndDate)
				assertions.True(event.End.After(*event.Start))
				return
			}
			assertions.Equal(tt.wantStart, event.Start.UTC().Format("20060102T150405Z"))
			assertions.Equal(tt.wantEnd, event.End.UTC().Format("20060102T150405Z"))
		})
	}
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

func TestICalendarSuccessiveSparseCancellationsPreserveInheritedDetails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	description := "Retained details " + strings.Repeat("x", 5000)
	request := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nDESCRIPTION:"+description+"\r\nLOCATION:North Room")
	first, err := st.PersistICalendarContext(t.Context(), source, request)
	requirements.NoError(err)
	id := first.MessageIDs[0]
	before := calendarRows(t, st, id)[0].Event
	assertions.True(before.DescriptionTruncated)
	assertions.Equal("North Room", before.Location)
	expectedBody, err := st.GetMessageBodyText(id)
	requirements.NoError(err)

	for _, sequence := range []string{"2", "3"} {
		cancel := calendarDocument("CANCEL", "SEQUENCE:"+sequence)
		_, err := st.PersistICalendarContext(t.Context(), source, cancel)
		requirements.NoError(err)
	}

	after := calendarRows(t, st, id)[0].Event
	assertions.Equal("cancelled", after.Status)
	assertions.Equal(before.Description, after.Description)
	assertions.True(after.DescriptionTruncated)
	assertions.Equal(before.Location, after.Location)
	body, err := st.GetMessageBodyText(id)
	requirements.NoError(err)
	assertions.Equal(expectedBody, body)
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
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	moved, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDTEND:20270303T150000Z"))
	requirements.NoError(err)
	var before sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, moved.MessageIDs[0]).Scan(&before))
	requirements.True(before.Valid)

	cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nRECURRENCE-ID:20270303T120000Z")
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	var after sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, moved.MessageIDs[0]).Scan(&after))
	assertions.Equal(before, after)
	event := calendarRows(t, st, moved.MessageIDs[0])[0].Event
	requirements.NotNil(event.Start)
	assertions.Equal(before.Time.UTC(), event.Start.UTC())
}

func TestICalendarMovedAllDayExceptionCancellationKeepsEffectiveSentAt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	moved, err := st.PersistICalendarContext(t.Context(), source,
		calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID;VALUE=DATE:20270303\r\nDTSTART;VALUE=DATE:20270305\r\nDTEND;VALUE=DATE:20270306"))
	requirements.NoError(err)
	var before sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, moved.MessageIDs[0]).Scan(&before))
	requirements.True(before.Valid)
	assertions.Equal(time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC), before.Time.UTC())

	cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nRECURRENCE-ID;VALUE=DATE:20270303")
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	var after sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, moved.MessageIDs[0]).Scan(&after))
	assertions.Equal(before, after)
	event := calendarRows(t, st, moved.MessageIDs[0])[0].Event
	assertions.True(event.AllDay)
	assertions.Equal("2027-03-05", event.StartDate)
}

func TestICalendarRetainsFamilyTimezoneBeforeParsingCivilDates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := strings.Replace(string(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART;VALUE=DATE:20111230\r\nDTEND;VALUE=DATE:20111231")),
		"UID:family@example.com", "UID:retained-zone@example.test", 1)
	first, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
	requirements.NoError(err)
	requirements.Len(first.MessageIDs, 1)

	_, err = st.db.Exec(`UPDATE sources SET sync_config=? WHERE id=?`, `{"time_zone":"Pacific/Apia"}`, source)
	requirements.NoError(err)
	reimported, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
	requirements.NoError(err)
	assertions.Equal(first.MessageIDs, reimported.MessageIDs)
	assertions.Equal(1, reimported.Unchanged)
	event := calendarRows(t, st, first.MessageIDs[0])[0].Event
	assertions.Equal("UTC", event.CalendarTimeZone)
	assertions.Equal("2011-12-30", event.StartDate)
}

func TestReadStoredICSParsesOnlySelectedEventInRetainedTimeZone(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:retained-date@example.test\r\nDTSTART;VALUE=DATE:20111230\r\nDTEND;VALUE=DATE:20111231\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:apias-event@example.test\r\nDTSTART:20120102T090000\r\nDTEND:20120102T100000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
	imported, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(imported.MessageIDs, 2)
	selectedID := imported.MessageIDs[1]

	var metadata string
	requirements.NoError(st.db.QueryRow(`SELECT metadata FROM messages WHERE id=?`, selectedID).Scan(&metadata))
	var meta calevent.ICSMetadata
	requirements.NoError(json.Unmarshal([]byte(metadata), &meta))
	meta.TimeZone = "Pacific/Apia"
	metadataBytes, err := json.Marshal(meta)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET metadata=? WHERE id=?`, string(metadataBytes), selectedID)
	requirements.NoError(err)

	event, component, _, err := readStoredICS(st.db, selectedID)
	requirements.NoError(err)
	requirements.Equal("apias-event@example.test", event.ICalUID)
	requirements.Equal("apias-event@example.test", component.Event.ICalUID)
	requirements.Equal("Pacific/Apia", event.EventTimeZone)
}

func TestICalendarSingleMasterCancellationPropagatesToDetachedExceptions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	child, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Single master"))
	requirements.NoError(err)
	assertions.Equal("confirmed", calendarRows(t, st, child.MessageIDs[0])[0].Event.Status)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTAMP:20270301T130000Z"))
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, child.MessageIDs[0])[0].Event.Status)
}

func TestICalendarFamilyCancellation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	master, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:3"))
	requirements.NoError(err)
	childRaw := calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID:20270302T120000Z\r\nDTSTART:20270302T140000Z\r\nDTEND:20270302T150000Z")
	child, err := st.PersistICalendarContext(t.Context(), source, childRaw)
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, child.MessageIDs[0])[0].Event.Status)
	assertions.Equal("series", calendarRows(t, st, master.MessageIDs[0])[0].Event.Kind)
	ownCancelled, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2\r\nRECURRENCE-ID:20270303T120000Z"))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:4\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nRRULE:FREQ=DAILY;COUNT=2"))
	requirements.NoError(err)
	assertions.Equal("confirmed", calendarRows(t, st, child.MessageIDs[0])[0].Event.Status)
	assertions.Equal("cancelled", calendarRows(t, st, ownCancelled.MessageIDs[0])[0].Event.Status)
	alias, err := func() (OccurrenceRef, error) {
		ctx, release, err := st.BeginReadSnapshotContext(t.Context())
		if err != nil {
			return OccurrenceRef{}, err
		}
		defer release()
		return resolveOccurrenceRefAlias(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: child.MessageIDs[0]})
	}()
	requirements.NoError(err)
	assertions.Equal(master.MessageIDs[0], alias.MessageID)
	assertions.Equal("20270302T120000Z", alias.Key)
}

func TestICalendarFamilyCancellationIncludesNonpositiveExceptionIDs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	master := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3")
	_, err := st.PersistICalendarContext(t.Context(), source, master)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source, "signed-exceptions", "Signed exceptions")
	requirements.NoError(err)
	insertException := func(id int64, sourceMessageID, date string) {
		raw := calendarDocument("REQUEST", "RECURRENCE-ID:"+date+"T120000Z\r\nDTSTART:"+date+"T140000Z\r\nDURATION:PT1H")
		index := 0
		metadata, err := json.Marshal(calevent.ICSMetadata{
			Version: 1, Index: &index, Namespace: "signed-exceptions", Method: "REQUEST", TimeZone: "UTC",
		})
		requirements.NoError(err)
		insertMessage := `INSERT INTO messages (id,conversation_id,source_id,source_message_id,message_type,metadata) VALUES (?,?,?,?,?,?)`
		if st.IsPostgreSQL() {
			insertMessage = `INSERT INTO messages (id,conversation_id,source_id,source_message_id,message_type,metadata)
				OVERRIDING SYSTEM VALUE VALUES (?,?,?,?,?,?)`
		}
		_, err = st.db.Exec(insertMessage,
			id, conversationID, source, sourceMessageID, calevent.MessageType, string(metadata))
		requirements.NoError(err)
		_, err = st.db.Exec(`INSERT INTO message_raw (message_id,raw_data,raw_format,compression) VALUES (?,?,?,?)`,
			id, raw, "ics", "")
		requirements.NoError(err)
		requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
			return st.refreshCalendarEventProjectionWith(t.Context(), tx, id)
		}))
	}
	insertException(-5, "negative-exception", "20270303")
	insertException(0, "zero-exception", "20270304")

	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2"))
	requirements.NoError(err)
	rows := calendarRows(t, st, -5, 0)
	requirements.Len(rows, 2)
	assertions.Equal("cancelled", rows[0].Event.Status)
	assertions.Equal("cancelled", rows[1].Event.Status)

	restore := calendarDocument("REQUEST", "SEQUENCE:3\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3")
	_, err = st.PersistICalendarContext(t.Context(), source, restore)
	requirements.NoError(err)
	rows = calendarRows(t, st, -5, 0)
	requirements.Len(rows, 2)
	assertions.Equal("confirmed", rows[0].Event.Status)
	assertions.Equal("confirmed", rows[1].Event.Status)
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
		assertions.Equal(multi, stored)
	}
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	hidden, err := calendarEventRowsByMessageIDs(ctx, st.snapshotReader(ctx), SourceScope{IDs: []int64{source + 1}}, sum.MessageIDs)
	release()
	requirements.NoError(err)
	assertions.Empty(hidden)
	invalid := strings.Replace(string(multi), "UID:second@example.org", "UID:", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(invalid))
	requirements.Error(err)
	statements := []string{
		`CREATE TRIGGER reject_calendar_uid BEFORE INSERT ON calendar_events WHEN NEW.ical_uid='blocked@example.org' BEGIN SELECT RAISE(ABORT, 'calendar projection blocked'); END`,
	}
	if st.IsPostgreSQL() {
		statements = []string{
			`CREATE FUNCTION reject_calendar_uid() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.ical_uid='blocked@example.org' THEN RAISE EXCEPTION 'calendar projection blocked'; END IF; RETURN NEW; END $$`,
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
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.source_id=? AND e.ical_uid IN (?,?)`, source, "first-new@example.org", "blocked@example.org").Scan(&count))
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
	assertions.Equal("20270303T140000Z", calendarRows(t, st, first.MessageIDs[0])[0].Event.Original.Key())
	assertions.Equal("series", calendarRows(t, st, master.MessageIDs[0])[0].Event.Kind)
}
func TestICalendarConcurrentRevisions(t *testing.T) {
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
		require.NoError(t, err)
	}
	var id int64
	require.NoError(t, st.db.QueryRow(`SELECT id FROM messages WHERE source_id=?`, source).Scan(&id))
	assert.Equal(t, int64(2), calendarRows(t, st, id)[0].Event.Sequence)
}
func TestICalendarRevisionStampAndLimits(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z")
	first, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	later := strings.Replace(string(raw), "20270301T120000Z", "20270301T130000Z", 1)
	later = strings.Replace(later, "20270302T120000Z", "20270302T140000Z", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	assertions.Equal("2027-03-02T14:00:00Z", calendarRows(t, st, first.MessageIDs[0])[0].Event.Start.Format("2006-01-02T15:04:05Z"))
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:1\r\nDTSTAMP:20270301T130000Z"))
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, first.MessageIDs[0])[0].Event.Status)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, first.MessageIDs[0])[0].Event.Status)
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
