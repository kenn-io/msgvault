package calevent

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/calrecur"
)

const calDAVFixture = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:series@example.org\r\nSUMMARY:Planning\\, session\r\nDTSTART;TZID=America/New_York:20270307T090000\r\nDTEND;TZID=America/New_York:20270307T100000\r\nRRULE:FREQ=WEEKLY;COUNT=3\r\nEXDATE;TZID=America/New_York:20270314T090000\r\nORGANIZER;CN=Example Organizer:mailto:owner@example.org\r\nATTENDEE;CN=Example Room;CUTYPE=ROOM:mailto:room@example.org\r\nATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:series@example.org\r\nRECURRENCE-ID:20270321T130000Z\r\nDTSTART:20270321T150000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestCalDAVCompoundProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	sc := "America/New_York"
	events, err := ParseCalDAVResource(t.Context(), []byte(calDAVFixture), sc)
	requirements.NoError(err)
	requirements.Len(events, 2)
	master := events[0]
	assertions.Equal("Planning, session", master.Summary)
	assertions.Equal("series", master.Event.Kind)
	assertions.Equal(time.Date(2027, 3, 7, 14, 0, 0, 0, time.UTC), *master.Event.Start)
	set, err := calrecur.ParseSet(*master.Event.Start, false, "", mustZone(t, "America/New_York"), master.Event.Recurrence)
	requirements.NoError(err)
	assertions.Len(set.ExDates, 1)
	assertions.Equal("exception", events[1].Event.Kind)
	assertions.Equal(master.Event.ProviderEventID, events[1].Event.SeriesProviderID)
	assertions.Equal("20270321T130000Z", events[1].Event.Original.Key())
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	requirements := require.New(t)
	zone, err := time.LoadLocation(name)
	requirements.NoError(err)
	return zone
}

func TestCalDAVRejectsInvalidUTF8(t *testing.T) {
	raw := strings.Replace(calDAVFixture, "SUMMARY:Planning\\, session", "SUMMARY:invalid\xfftext", 1)
	events, err := ParseCalDAVResource(t.Context(), []byte(raw), "UTC")
	require.ErrorContains(t, err, "UTF-8")
	assert.Empty(t, events)
}

func TestCalDAVDatesCancellationAndInvalidContent(t *testing.T) {
	wrap := func(event string) []byte {
		return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + event + "END:VCALENDAR\r\n")
	}
	for _, tc := range []struct {
		name, body, wantErr string
		check               func(*testing.T, []CalDAVEvent)
	}{
		{"all-day DST", "BEGIN:VEVENT\r\nUID:day\r\nDTSTART;VALUE=DATE:20270314\r\nEND:VEVENT\r\n", "", func(t *testing.T, events []CalDAVEvent) {
			t.Helper()
			require.Len(t, events, 1)
			e := events[0].Event
			assert.True(t, e.AllDay)
			assert.Equal(t, "2027-03-15", e.EndDate)
			assert.Equal(t, 23*time.Hour, e.End.Sub(*e.Start))
		}},
		{"cancelled sparse", "BEGIN:VEVENT\r\nUID:day\r\nRECURRENCE-ID;VALUE=DATE:20270314\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n", "", func(t *testing.T, events []CalDAVEvent) {
			t.Helper()
			require.Len(t, events, 1)
			assert.Nil(t, events[0].Event.Start)
			assert.Equal(t, "cancelled", events[0].Event.Status)
			assert.Equal(t, "20270314", events[0].Event.Original.Key())
		}},
		{"task only", "BEGIN:VTODO\r\nUID:task\r\nEND:VTODO\r\n", "", nil},
		{"missing live start", "BEGIN:VEVENT\r\nUID:bad\r\nEND:VEVENT\r\n", "CalDAV live event has no DTSTART", nil},
		{"range", "BEGIN:VEVENT\r\nUID:bad\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270314T130000Z\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n", "CalDAV recurrence RANGE is unsupported", nil},
		{"custom zone", "BEGIN:VEVENT\r\nUID:bad\r\nDTSTART;TZID=Custom/Zone:20270314T130000\r\nEND:VEVENT\r\n", "unknown time zone", nil},
		{"malformed parameter", "BEGIN:VEVENT\r\nUID;X=\r\nEND:VEVENT\r\n", "malformed CalDAV iCalendar content", nil},
		{"nested", "BEGIN:VEVENT\r\nUID:outer\r\nDTSTART:20270314T130000Z\r\nBEGIN:VEVENT\r\nUID:inner\r\nEND:VEVENT\r\nEND:VEVENT\r\n", "unsupported nested CalDAV event component", nil},
		{"all-day end equals start", "BEGIN:VEVENT\r\nUID:bad\r\nDTSTART;VALUE=DATE:20270314\r\nDTEND;VALUE=DATE:20270314\r\nEND:VEVENT\r\n", "all-day DTEND must follow DTSTART", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			events, err := ParseCalDAVResource(t.Context(), wrap(tc.body), "America/New_York")
			if tc.wantErr != "" {
				requirements.ErrorContains(err, tc.wantErr)
				return
			}
			requirements.NoError(err)
			if tc.check == nil {
				assertions.Empty(events)
				return
			}
			tc.check(t, events)
		})
	}
}

func TestCalDAVRequiresVersion2(t *testing.T) {
	for _, version := range []string{"VERSION:3.0\r\n", ""} {
		raw := "BEGIN:VCALENDAR\r\n" + version + "BEGIN:VEVENT\r\nUID:version@example.org\r\nDTSTART:20270314T130000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		_, err := ParseCalDAVResource(t.Context(), []byte(raw), "UTC")
		require.ErrorContains(t, err, "VERSION 2.0")
	}
}

func TestCalDAVAllDayDatesPreserveSkippedCivilDates(t *testing.T) {
	for _, tc := range []struct {
		name, zone, start, end, extra, override      string
		startDate, endDate, startInstant, endInstant string
	}{
		{
			name: "Sao Paulo midnight", zone: "America/Sao_Paulo", start: "20181104", end: "DTEND;VALUE=DATE:20181105\r\n",
			extra: "RRULE:FREQ=DAILY;COUNT=2\r\n", override: "BEGIN:VEVENT\r\nUID:skipped@example.org\r\nRECURRENCE-ID;VALUE=DATE:20181104\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n",
			startDate: "2018-11-04", endDate: "2018-11-05", startInstant: "2018-11-04T03:00:00Z", endInstant: "2018-11-05T02:00:00Z",
		},
		{name: "Apia explicit", zone: "Pacific/Apia", start: "20111229", end: "DTEND;VALUE=DATE:20111230\r\n", startDate: "2011-12-29", endDate: "2011-12-30", startInstant: "2011-12-29T10:00:00Z", endInstant: "2011-12-30T10:00:00Z"},
		{name: "Apia duration", zone: "Pacific/Apia", start: "20111229", end: "DURATION:P1D\r\n", startDate: "2011-12-29", endDate: "2011-12-30", startInstant: "2011-12-29T10:00:00Z", endInstant: "2011-12-30T10:00:00Z"},
		{name: "Apia default", zone: "Pacific/Apia", start: "20111229", startDate: "2011-12-29", endDate: "2011-12-30", startInstant: "2011-12-29T10:00:00Z", endInstant: "2011-12-30T10:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:skipped@example.org\r\nDTSTART;VALUE=DATE:" + tc.start + "\r\n" + tc.end + tc.extra + "END:VEVENT\r\n" + tc.override + "END:VCALENDAR\r\n")
			events, err := ParseCalDAVResource(t.Context(), raw, tc.zone)
			requirements.NoError(err)
			wantCount := 1
			if tc.override != "" {
				wantCount = 2
			}
			requirements.Len(events, wantCount)
			event := events[0].Event
			assertions.True(event.AllDay)
			assertions.Equal(tc.startDate, event.StartDate)
			assertions.Equal(tc.endDate, event.EndDate)
			assertions.Equal(tc.startInstant, event.Start.UTC().Format(time.RFC3339))
			assertions.Equal(tc.endInstant, event.End.UTC().Format(time.RFC3339))
			if tc.override != "" {
				exception := events[1].Event
				requirements.NotNil(exception.Original)
				assertions.Equal("2018-11-04", exception.Original.Date)
				assertions.True(exception.Original.AllDay)
			}
		})
	}
}

func TestCalDAVPreservesUnescapedCommasInTextProperties(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:comma-text@example.org\r\nDTSTART:20270314T130000Z\r\nSUMMARY:Standup, weekly\r\nDESCRIPTION:Budget, monthly\r\nLOCATION:Room, west\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	events, err := ParseCalDAVResource(t.Context(), raw, "")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Equal("Standup, weekly", events[0].Summary)
	assertions.Equal("Budget, monthly", events[0].Description)
	assertions.Equal("Room, west", events[0].Location)
}

func TestCalDAVPreservesUnsupportedExRuleForProjectionValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:exrule@example.org\r\nDTSTART:20270314T130000Z\r\nRRULE:FREQ=DAILY;COUNT=2\r\nEXRULE:FREQ=DAILY;COUNT=1\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	events, err := ParseCalDAVResource(t.Context(), raw, "")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Event.Recurrence, "EXRULE:FREQ=DAILY;COUNT=1")
}

func TestCalDAVRejectsHostLocalTimeZoneParameters(t *testing.T) {
	for _, tc := range []struct{ name, properties string }{
		{"DTSTART", "DTSTART;TZID=Local:20270314T130000"},
		{"DTEND", "DTSTART:20270314T130000Z\r\nDTEND;TZID=Local:20270314T140000"},
		{"RECURRENCE-ID", "RECURRENCE-ID;TZID=Local:20270314T130000\r\nSTATUS:CANCELLED"},
		{"RDATE", "DTSTART:20270314T130000Z\r\nRDATE;TZID=Local:20270321T130000"},
		{"EXDATE", "DTSTART:20270314T130000Z\r\nEXDATE;TZID=Local:20270321T130000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:local-tz@example.org\r\n" + tc.properties + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
			_, err := ParseCalDAVResource(t.Context(), raw, "")
			require.ErrorContains(t, err, "Local")
		})
	}
}

func TestCalDAVEmailDecodesMailtoMailboxOnce(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"mailto:Guest%2BCalendar@Example.org", "guest+calendar@example.org"},
		{"mailto:Guest%2BCalendar@Example.org?subject=Ignored", "guest+calendar@example.org"},
		{"mailto:Guest%252BCalendar@Example.org", "guest%2bcalendar@example.org"},
		{"mailto:Guest%ZZ@example.org", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, calDAVEmail(&ical.Prop{Value: tc.input}))
		})
	}
}

func TestCalDAVResourceSizeLimitRejectsFoldedContentWithShortPhysicalLines(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const resourceLimit = 2 << 20
	chunk := strings.Repeat("x", 15<<10)
	var oversized strings.Builder
	oversized.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:oversized@example.org\r\nDTSTART:20270314T130000Z\r\nDESCRIPTION:")
	oversized.WriteString(chunk)
	for oversized.Len() <= resourceLimit {
		oversized.WriteString("\r\n ")
		oversized.WriteString(chunk)
	}
	oversized.WriteString("\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	raw := oversized.String()
	requirements.Greater(len(raw), resourceLimit)
	maxPhysicalLine := 0
	for line := range strings.SplitSeq(raw, "\r\n") {
		maxPhysicalLine = max(maxPhysicalLine, len(line))
	}
	assertions.Less(maxPhysicalLine, 16<<10)

	_, err := ParseCalDAVResource(t.Context(), []byte(raw), "")
	requirements.ErrorContains(err, "CalDAV resource exceeds 2 MiB")
}

func FuzzCalDAVDecoder(f *testing.F) {
	f.Add([]byte(calDAVFixture))
	f.Add([]byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID;X=\nEND:VEVENT\nEND:VCALENDAR\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		assertions := assert.New(t)
		requirements := require.New(t)
		events, err := ParseCalDAVResource(t.Context(), raw, "UTC")
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, e := range events {
			requirements.False(seen[e.Event.ProviderEventID])
			seen[e.Event.ProviderEventID] = true
			meta, err := json.Marshal(CalDAVMetadata{EventID: e.Event.ProviderEventID})
			requirements.NoError(err)
			decoded, ok, err := Decode(CalDAVRawFormat, raw, meta, "UTC")
			requirements.NoError(err)
			requirements.True(ok)
			assertions.Equal(e.Event, decoded)
		}
	})
}

func TestCalDAVCaseInsensitiveParametersAndCivilDuration(t *testing.T) {
	for _, tc := range []struct{ name, dtstart, duration, params, role, end string }{
		{"civil day across DST", "DTSTART;TZID=America/New_York:20270313T090000", "P1D", "CUTYPE=room", "room", "2027-03-14T13:00:00Z"},
		{"elapsed hours across DST", "DTSTART;TZID=America/New_York:20270313T090000", "PT24H", "ROLE=opt-participant", "optional", "2027-03-14T14:00:00Z"},
		{"lowercase DATE", "DTSTART;VALUE=date:20270314", "P1D", "CUTYPE=resource", "room", "2027-03-15T04:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:case@example.org\r\n" + tc.dtstart + "\r\nDURATION:" + tc.duration + "\r\nATTENDEE;" + tc.params + ":mailto:room@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
			events, err := ParseCalDAVResource(t.Context(), raw, "America/New_York")
			requirements.NoError(err)
			requirements.Len(events, 1)
			want, err := time.Parse(time.RFC3339, tc.end)
			requirements.NoError(err)
			assertions.Equal(want, *events[0].Event.End)
			assertions.Equal(tc.role, events[0].Event.Attendees[0].Role)
		})
	}
}

func TestCalDAVFloatingRecurrenceIdentityIgnoresFallbackZone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:floating@example.org\r\nRECURRENCE-ID:20270321T090000\r\nDTSTART:20270321T110000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	for _, zone := range []string{"UTC", "America/New_York"} {
		events, err := ParseCalDAVResource(t.Context(), raw, zone)
		require.NoError(err)
		require.Len(events, 1)
		assert.Equal(`["floating@example.org","FLOATING:20270321T090000"]`, events[0].Event.ProviderEventID)
	}
}

func TestCalDAVLocalTimesInGapAndFold(t *testing.T) {
	for _, tc := range []struct{ name, zone, start, original, want, key string }{
		{"recurring gap", "America/New_York", "20270314T023000", "RRULE:FREQ=WEEKLY;COUNT=2\r\n", "2027-03-14T07:30:00Z", ""},
		{"exception fold", "Europe/Berlin", "20271031T023000", "RECURRENCE-ID;TZID=Europe/Berlin:20271031T023000\r\n", "2027-10-31T00:30:00Z", "20271031T003000Z"},
		{"UTC overrides zone", "Europe/Berlin", "20271031T023000Z", "", "2027-10-31T02:30:00Z", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:clock@example.org\r\nDTSTART;TZID=" + tc.zone + ":" + tc.start + "\r\nDTEND;TZID=" + tc.zone + ":" + tc.start + "\r\n" + tc.original + "END:VEVENT\r\nEND:VCALENDAR\r\n")
			// Both explicit TZID and the configured calendar zone follow the same rule.
			for _, input := range [][]byte{raw, []byte(strings.ReplaceAll(string(raw), ";TZID="+tc.zone, ""))} {
				events, err := ParseCalDAVResource(t.Context(), input, tc.zone)
				requirements.NoError(err)
				requirements.Len(events, 1)
				assertions.Equal(tc.want, events[0].Event.Start.Format(time.RFC3339))
				assertions.Equal(tc.want, events[0].Event.End.Format(time.RFC3339))
				if tc.name == "recurring gap" {
					assertions.Contains(events[0].Event.Recurrence, "DTSTART:20270314T023000")
				}
				if tc.key != "" {
					requirements.NotNil(events[0].Event.Original)
					assertions.Equal(tc.key, events[0].Event.Original.Key())
					if strings.Contains(string(input), ";TZID=") {
						assertions.Equal(`["clock@example.org","TZID=Europe/Berlin:20271031T023000"]`, events[0].Event.ProviderEventID)
					} else {
						assertions.Equal(`["clock@example.org","FLOATING:20271031T023000"]`, events[0].Event.ProviderEventID)
					}
				}
			}
		})
	}
}

func TestCalDAVAttendeeRolesAndRSVP(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:roles@example.org\r\nDTSTART:20270314T130000Z\r\n" +
		"ORGANIZER;CN=Alice:mailto:alice@example.org\r\nATTENDEE;ROLE=CHAIR:mailto:bob@example.org\r\nATTENDEE:mailto:carol@example.org\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:dave@example.org\r\nATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nATTENDEE;ROLE=NON-PARTICIPANT:mailto:info@example.org\r\nATTENDEE:urn:uuid:00000000-0000-0000-0000-000000000000\r\nATTENDEE;CUTYPE=ROOM;ROLE=OPT-PARTICIPANT:mailto:room@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	event, ok, err := Decode(CalDAVRawFormat, raw, []byte(`{"event_id":"[\"roles@example.org\",\"\"]","account_email":"carol@example.org"}`), "UTC")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []Attendee{
		{Email: "bob@example.org", Role: "required", RSVP: "needs_action"},
		{Email: "carol@example.org", Role: "required", RSVP: "needs_action", IsCopySelf: true},
		{Email: "dave@example.org", Role: "required", RSVP: "needs_action"},
		{Email: "guest@example.org", Role: "optional", RSVP: "accepted"},
		{Email: "info@example.org", Role: "optional", RSVP: "needs_action"},
		{Role: "required", RSVP: "needs_action"},
		{Email: "room@example.org", Role: "room", RSVP: "needs_action"},
		{Email: "alice@example.org", DisplayName: "Alice", Role: "organizer", RSVP: "unknown"},
	}, event.Attendees)
}

func TestCalDAVRejectsComponentLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	for i := range 1001 {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:event-%d\r\nDTSTART:20270314T130000Z\r\nEND:VEVENT\r\n", i)
	}
	b.WriteString("END:VCALENDAR\r\n")
	_, err := ParseCalDAVResource(t.Context(), []byte(b.String()), "UTC")
	assert.ErrorContains(t, err, "CalDAV resource exceeds 1000 components")
}

func TestCalDAVAllDayExceptionKeepsCalendarZone(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:day@example.org\r\nRECURRENCE-ID;VALUE=DATE:20270321\r\nDTSTART;VALUE=DATE:20270322\r\nDTEND;VALUE=DATE:20270323\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	events, err := ParseCalDAVResource(t.Context(), []byte(raw), "Asia/Tokyo")
	requirements.NoError(err)
	requirements.Len(events, 1)
	e := events[0].Event
	assertions.Equal("Asia/Tokyo", e.EventTimeZone)
	assertions.Equal("2027-03-22", e.StartDate)
	assertions.Equal("2027-03-23", e.EndDate)
}

func TestCalDAVHistoricalEventUsesIANATimeZone(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nBEGIN:DAYLIGHT\r\nDTSTART:20070311T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nEND:DAYLIGHT\r\nBEGIN:STANDARD\r\nDTSTART:20071104T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:historical@example.org\r\nDTSTART;TZID=America/New_York:20050615T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	events, err := ParseCalDAVResource(t.Context(), []byte(raw), "UTC")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, time.Date(2005, 6, 15, 13, 0, 0, 0, time.UTC), *events[0].Event.Start)
}

func TestCalDAVCaseInsensitiveRecurrence(t *testing.T) {
	for _, tc := range []struct {
		name, rule string
		want       []string
	}{
		{"rule", "RRULE:freq=weekly;count=2;byday=su\r\n", []string{"RRULE:FREQ=WEEKLY;COUNT=2;BYDAY=SU"}},
		{"UTC dates", "RRULE:freq=weekly;count=2\r\nEXDATE;VALUE=date-time:20270321t130000z\r\nRDATE;VALUE=date-time:20270328t130000z\r\n", []string{"RRULE:FREQ=WEEKLY;COUNT=2", "RDATE;VALUE=DATE-TIME:20270328T130000Z", "EXDATE;VALUE=DATE-TIME:20270321T130000Z"}},
		{"local dates", "RRULE:freq=weekly;count=2\r\nEXDATE;TZID=America/New_York;VALUE=date-time:20270321t090000\r\nRDATE;TZID=America/New_York;VALUE=date-time:20270328t090000\r\n", []string{"RRULE:FREQ=WEEKLY;COUNT=2", "RDATE;TZID=America/New_York;VALUE=DATE-TIME:20270328T090000", "EXDATE;TZID=America/New_York;VALUE=DATE-TIME:20270321T090000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:case@example.org\r\nDTSTART:20270314T130000Z\r\n" + tc.rule + "END:VEVENT\r\nEND:VCALENDAR\r\n"
			events, err := ParseCalDAVResource(t.Context(), []byte(raw), "UTC")
			require.NoError(t, err)
			require.Len(t, events, 1)
			assert.Equal(t, tc.want, events[0].Event.Recurrence)
		})
	}
}
