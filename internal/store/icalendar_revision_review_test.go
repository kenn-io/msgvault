package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestICalendarCancelledRetainedSnapshotRejectsOldRecurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	old := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:Old series")
	accepted := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270304T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Accepted single event")
	for _, raw := range [][]byte{old, accepted, calendarDocument("CANCEL", "SEQUENCE:3\r\nSTATUS:CANCELLED")} {
		_, err := st.PersistICalendarContext(t.Context(), source, raw)
		requirements.NoError(err)
	}
	replay, err := st.PersistICalendarContext(t.Context(), source, old)
	requirements.NoError(err)
	requirements.Len(replay.MessageIDs, 1)
	stored, err := st.GetMessageRaw(replay.MessageIDs[0])
	requirements.NoError(err)
	assertions.Equal(accepted, stored)
	event := calendarRows(t, st, replay.MessageIDs[0])[0].Event
	assertions.Empty(event.Recurrence)
	requirements.NotNil(event.Start)
	assertions.Equal("2027-03-04T14:00:00Z", event.Start.Format(time.RFC3339))
	assertions.Equal("cancelled", event.Status)
}

func TestICalendarRejectedRevisionKeepsAcceptedMessageTime(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	accepted := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270310T090000\r\nDURATION:PT1H")
	first, err := st.PersistICalendarContext(t.Context(), source, accepted)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"America/New_York"}`))
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270301T180000\r\nDURATION:PT1H"))
	requirements.NoError(err)
	var sent, last time.Time
	requirements.NoError(st.db.QueryRow(`SELECT m.sent_at,c.last_message_at FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?`, first.MessageIDs[0]).Scan(&sent, &last))
	assertions.Equal("2027-03-10T14:00:00Z", sent.UTC().Format(time.RFC3339))
	assertions.Equal(sent.UTC(), last.UTC())
	stored, err := st.GetMessageRaw(first.MessageIDs[0])
	requirements.NoError(err)
	assertions.Equal(accepted, stored)
}

func TestEmailCalendarInitialSnapshotWithoutOrganizer(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "initial", inviteEmail(calendarFamilyDocument()))
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 2)
	event := calendarRows(t, st, ids[1])[0].Event
	requirements.NotNil(event.Start)
	assertions.Equal("2027-03-03T14:00:00Z", event.Start.Format(time.RFC3339))
	persistInviteEmail(t, st, "later", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:2\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nDURATION:PT1H")))
	event = calendarRows(t, st, ids[1])[0].Event
	assertions.Equal("2027-03-03T14:00:00Z", event.Start.Format(time.RFC3339))
}

func TestICalendarExceptionSupersessionUsesStamp(t *testing.T) {
	for _, mode := range []string{"omitted", "included", "late", "replay"} {
		t.Run(mode, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			initial := strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nSEQUENCE:1\r\nDTSTAMP:20270301T090000Z\r\n")
			exception := strings.Split(initial, "BEGIN:VEVENT")[2]
			detached := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT" + exception)
			if mode != "late" {
				_, err := st.PersistICalendarContext(t.Context(), source, []byte(initial))
				requirements.NoError(err)
			}
			master := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTAMP:20270301T100000Z\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2")
			if mode == "included" {
				master = []byte(strings.Replace(string(master), "END:VCALENDAR\r\n", "BEGIN:VEVENT"+exception, 1))
			}
			_, err := st.PersistICalendarContext(t.Context(), source, master)
			requirements.NoError(err)
			if mode == "late" || mode == "replay" {
				_, err = st.PersistICalendarContext(t.Context(), source, detached)
				requirements.NoError(err)
			}
			var metadata string
			requirements.NoError(st.db.QueryRow(`SELECT metadata FROM messages WHERE source_id=? AND source_message_id<>?`, source, calevent.ICSMessageID("family@example.com", "")).Scan(&metadata))
			meta, err := calevent.ParseICSMetadata([]byte(metadata))
			requirements.NoError(err)
			assertions.Equal(mode != "included", meta.Superseded)
			from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 0, 1)})
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			wantHour := 12
			if mode == "included" {
				wantHour = 14
			}
			assertions.Equal(wantHour, page.Occurrences[0].Start.Hour())
		})
	}
}

func TestICalendarExceptionAliasPrefersNewerUTCRevisionOverOlderFloatingAlias(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	requirements.NoError(st.UpdateSourceSyncConfig(source, "{\"time_zone\":\"UTC\"}"))

	master := calendarDocument("REQUEST", "SEQUENCE:0\r\nDTSTAMP:20270301T090000Z\r\n"+
		"DTSTART:20270310T160000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=1\r\nSUMMARY:Series")
	_, err := st.PersistICalendarContext(t.Context(), source, master)
	requirements.NoError(err)

	newerUTC := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270302T090000Z\r\n"+
		"RECURRENCE-ID:20270310T160000Z\r\nDTSTART:20270310T180000Z\r\nDURATION:PT1H\r\nSUMMARY:Newer UTC revision")
	newer, err := st.PersistICalendarContext(t.Context(), source, newerUTC)
	requirements.NoError(err)
	requirements.Len(newer.MessageIDs, 1)

	olderFloating := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTAMP:20270301T080000Z\r\n"+
		"RECURRENCE-ID:20270310T160000\r\nDTSTART:20270310T100000\r\nDURATION:PT1H\r\nSUMMARY:Older floating revision")
	older, err := st.PersistICalendarContext(t.Context(), source, olderFloating)
	requirements.NoError(err)
	requirements.Len(older.MessageIDs, 1)
	assertions.NotEqual(newer.MessageIDs[0], older.MessageIDs[0], "UTC and floating recurrence IDs retain distinct source identities")
	aliases := calendarRows(t, st, newer.MessageIDs[0], older.MessageIDs[0])
	requirements.Len(aliases, 2)
	aliasRows := make(map[int64]CalendarEventRow, len(aliases))
	for _, row := range aliases {
		aliasRows[row.MessageID] = row
	}
	newerAlias, newerFound := aliasRows[newer.MessageIDs[0]]
	olderAlias, olderFound := aliasRows[older.MessageIDs[0]]
	requirements.True(newerFound)
	requirements.True(olderFound)
	requirements.NotNil(newerAlias.Event.Original)
	requirements.NotNil(olderAlias.Event.Original)
	assertions.Equal(newerAlias.Event.Original.Key(), olderAlias.Event.Original.Key(), "UTC and floating identities alias the same occurrence")
	assertions.True(calendarEventSnapshotNewer(newerAlias, olderAlias), "the later observed alias has an older ICS revision")

	from := time.Date(2027, time.March, 10, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 0, 1)})
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(newer.MessageIDs[0], page.Occurrences[0].MessageID)
	assertions.Equal("2027-03-10T18:00:00Z", page.Occurrences[0].Start.UTC().Format(time.RFC3339))
}

func TestCalendarEventSnapshotNewerUsesICSRevisionBeforeObservationOrder(t *testing.T) {
	baseStamp := time.Date(2027, time.March, 1, 9, 0, 0, 0, time.UTC)
	laterStamp := baseStamp.Add(time.Hour)
	row := func(sequence int64, stamp time.Time, status string, order int64) CalendarEventRow {
		return CalendarEventRow{
			ProviderObservationOrder: order,
			rawFormat:                rawFormatICS,
			icsMetadata: calevent.ICSMetadata{
				Version:  1,
				Sequence: sequence,
				Stamp:    stamp,
				Status:   status,
			},
		}
	}
	for _, tc := range []struct {
		name      string
		candidate CalendarEventRow
		current   CalendarEventRow
		want      bool
	}{
		{name: "sequence precedes stamp and observation order", candidate: row(2, baseStamp, "confirmed", 1), current: row(1, laterStamp, "confirmed", 2), want: true},
		{name: "older sequence stays older despite later stamp and observation", candidate: row(1, laterStamp, "confirmed", 3), current: row(2, baseStamp, "confirmed", 2), want: false},
		{name: "stamp precedes observation order", candidate: row(1, laterStamp, "confirmed", 1), current: row(1, baseStamp, "confirmed", 2), want: true},
		{name: "cancellation wins an exact revision tie", candidate: row(1, baseStamp, "cancelled", 1), current: row(1, baseStamp, "confirmed", 2), want: true},
		{name: "observation order breaks an exact revision tie", candidate: row(1, baseStamp, "confirmed", 2), current: row(1, baseStamp, "confirmed", 1), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, calendarEventSnapshotNewer(tc.candidate, tc.current))
		})
	}
}
