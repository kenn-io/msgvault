package calevent

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
	"strings"
	"testing"
	"time"
)

const calDAVFixture = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:series@example.org\r\nSUMMARY:Planning\\, session\r\nDTSTART;TZID=America/New_York:20270307T090000\r\nDTEND;TZID=America/New_York:20270307T100000\r\nRRULE:FREQ=WEEKLY;COUNT=3\r\nEXDATE;TZID=America/New_York:20270314T090000\r\nORGANIZER;CN=Example Organizer:mailto:owner@example.org\r\nATTENDEE;CN=Example Room;CUTYPE=ROOM:mailto:room@example.org\r\nATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:series@example.org\r\nRECURRENCE-ID:20270321T130000Z\r\nDTSTART:20270321T150000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestCalDAVCompoundProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	sc := SourceContext{CalendarID: "calendar", CalendarTimeZone: "America/New_York"}
	events, err := ParseCalDAVResource([]byte(calDAVFixture), sc)
	requirements.NoError(err)
	requirements.Len(events, 2)
	master := events[0]
	assertions.Equal("Planning, session", master.Summary)
	assertions.Equal("series", master.Event.Kind)
	assertions.Equal("room", master.Event.Attendees[0].Role)
	assertions.Equal("optional", master.Event.Attendees[1].Role)
	assertions.Equal("accepted", master.Event.Attendees[1].RSVP)
	assertions.Equal(time.Date(2027, 3, 7, 14, 0, 0, 0, time.UTC), *master.Event.Start)
	set, err := calrecur.ParseSet(*master.Event.Start, false, "", mustZone(t, "America/New_York"), master.Event.Recurrence)
	requirements.NoError(err)
	assertions.Len(set.ExDates, 1)
	assertions.Equal("exception", events[1].Event.Kind)
	assertions.Equal(master.Event.ProviderEventID, events[1].Event.SeriesProviderID)
	assertions.Equal("20270321T130000Z", events[1].Event.Original.Key())
	for _, e := range events {
		meta, err := json.Marshal(CalDAVMetadata{EventID: e.Event.ProviderEventID, AccountEmail: "guest@example.org"})
		requirements.NoError(err)
		decoded, ok, err := Decode(CalDAVRawFormat, []byte(calDAVFixture), meta, sc)
		requirements.NoError(err)
		requirements.True(ok)
		assertions.Equal(e.Event.ProviderEventID, decoded.ProviderEventID)
	}
	_, _, err = Decode(CalDAVRawFormat, []byte(calDAVFixture), nil, sc)
	requirements.Error(err)
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	requirements := require.New(t)
	zone, err := time.LoadLocation(name)
	requirements.NoError(err)
	return zone
}

func TestCalDAVDatesCancellationAndInvalidContent(t *testing.T) {
	wrap := func(event string) []byte {
		return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + event + "END:VCALENDAR\r\n")
	}
	for _, tc := range []struct {
		name, body string
		fail       bool
	}{
		{"all-day DST", "BEGIN:VEVENT\r\nUID:day\r\nDTSTART;VALUE=DATE:20270314\r\nEND:VEVENT\r\n", false},
		{"cancelled sparse", "BEGIN:VEVENT\r\nUID:day\r\nRECURRENCE-ID;VALUE=DATE:20270314\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n", false},
		{"task only", "BEGIN:VTODO\r\nUID:task\r\nEND:VTODO\r\n", false},
		{"missing live start", "BEGIN:VEVENT\r\nUID:bad\r\nEND:VEVENT\r\n", true},
		{"range", "BEGIN:VEVENT\r\nUID:bad\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270314T130000Z\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n", true},
		{"custom zone", "BEGIN:VEVENT\r\nUID:bad\r\nDTSTART;TZID=Custom/Zone:20270314T130000\r\nEND:VEVENT\r\n", true},
		{"malformed parameter", "BEGIN:VEVENT\r\nUID;X=\r\nEND:VEVENT\r\n", true},
		{"nested", "BEGIN:VEVENT\r\nUID:outer\r\nDTSTART:20270314T130000Z\r\nBEGIN:VEVENT\r\nUID:inner\r\nEND:VEVENT\r\nEND:VEVENT\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			events, err := ParseCalDAVResource(wrap(tc.body), SourceContext{CalendarTimeZone: "America/New_York"})
			if tc.fail {
				requirements.Error(err)
				return
			}
			requirements.NoError(err)
			if tc.name == "all-day DST" {
				requirements.Len(events, 1)
				e := events[0].Event
				assertions.True(e.AllDay)
				assertions.Equal("2027-03-15", e.EndDate)
				assertions.Equal(23*time.Hour, e.End.Sub(*e.Start))
			}
			if tc.name == "cancelled sparse" {
				assertions.False(events[0].Event.HasBounds)
				assertions.Equal("cancelled", events[0].Event.Status)
				assertions.Equal("20270314", events[0].Event.Original.Key())
			}
		})
	}
}

func TestCalDAVAllDayDatesPreserveCivilDatesWhenMidnightIsSkipped(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	zone := mustZone(t, "America/Sao_Paulo")
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:skipped-midnight@example.org\r\n" +
		"DTSTART;VALUE=DATE:20181104\r\nDTEND;VALUE=DATE:20181105\r\n" +
		"RRULE:FREQ=DAILY;COUNT=2\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:skipped-midnight@example.org\r\n" +
		"RECURRENCE-ID;VALUE=DATE:20181104\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
	events, err := ParseCalDAVResource(raw, SourceContext{CalendarTimeZone: zone.String()})
	requirements.NoError(err)
	requirements.Len(events, 2)

	master := events[0].Event
	assertions.True(master.AllDay)
	assertions.Equal("2018-11-04", master.StartDate)
	assertions.Equal("2018-11-05", master.EndDate)
	assertions.Equal(time.Date(2018, time.November, 4, 1, 0, 0, 0, zone).UTC(), *master.Start)
	assertions.Equal(time.Date(2018, time.November, 5, 0, 0, 0, 0, zone).UTC(), *master.End)

	exception := events[1].Event
	requirements.NotNil(exception.Original)
	assertions.Equal("2018-11-04", exception.Original.Date)
	assertions.True(exception.Original.AllDay)
}

func TestCalDAVAllDayExclusiveEndPreservesWhollySkippedCivilDate(t *testing.T) {
	zone := mustZone(t, "Pacific/Apia")
	for _, tc := range []struct {
		name string
		end  string
	}{
		{name: "explicit", end: "DTEND;VALUE=DATE:20111230\r\n"},
		{name: "duration", end: "DURATION:P1D\r\n"},
		{name: "default", end: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
				"BEGIN:VEVENT\r\nUID:skipped-end@example.org\r\n" +
				"DTSTART;VALUE=DATE:20111229\r\n" + tc.end +
				"SUMMARY:Skipped exclusive date\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
			events, err := ParseCalDAVResource(raw, SourceContext{CalendarTimeZone: zone.String()})
			requirements.NoError(err)
			requirements.Len(events, 1)
			event := events[0].Event
			assertions.True(event.AllDay)
			assertions.Equal("2011-12-29", event.StartDate)
			assertions.Equal("2011-12-30", event.EndDate)
			assertions.Equal(time.Date(2011, time.December, 29, 0, 0, 0, 0, zone).UTC(), *event.Start)
			assertions.Equal(time.Date(2011, time.December, 31, 0, 0, 0, 0, zone).UTC(), *event.End)
		})
	}
}

func TestCalDAVPreservesUnescapedCommasInTextProperties(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:comma-text@example.org\r\nDTSTART:20270314T130000Z\r\nSUMMARY:Standup, weekly\r\nDESCRIPTION:Budget, monthly\r\nLOCATION:Room, west\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	events, err := ParseCalDAVResource(raw, SourceContext{})
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Equal("Standup, weekly", events[0].Summary)
	assertions.Equal("Budget, monthly", events[0].Event.Description)
	assertions.Equal("Room, west", events[0].Event.Location)
}

func TestCalDAVPreservesUnsupportedExRuleForProjectionValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:exrule@example.org\r\nDTSTART:20270314T130000Z\r\nRRULE:FREQ=DAILY;COUNT=2\r\nEXRULE:FREQ=DAILY;COUNT=1\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	events, err := ParseCalDAVResource(raw, SourceContext{})
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Event.Recurrence, "EXRULE:FREQ=DAILY;COUNT=1")
	_, err = calrecur.ParseSet(*events[0].Event.Start, false, "", time.UTC, events[0].Event.Recurrence)
	requirements.ErrorIs(err, calrecur.ErrUnsupportedRecurrence)
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
			_, err := ParseCalDAVResource(raw, SourceContext{})
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
			assert.Equal(t, tc.want, calDAVEmail(tc.input))
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

	_, err := ParseCalDAVResource([]byte(raw), SourceContext{})
	requirements.ErrorContains(err, "CalDAV resource exceeds 2 MiB")
}

func FuzzCalDAVDecoder(f *testing.F) {
	f.Add([]byte(calDAVFixture))
	f.Add([]byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID;X=\nEND:VEVENT\nEND:VCALENDAR\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		assertions := assert.New(t)
		requirements := require.New(t)
		events, err := ParseCalDAVResource(raw, SourceContext{CalendarID: "example", CalendarTimeZone: "UTC"})
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, e := range events {
			requirements.False(seen[e.Event.ProviderEventID])
			seen[e.Event.ProviderEventID] = true
			meta, err := json.Marshal(CalDAVMetadata{EventID: e.Event.ProviderEventID})
			requirements.NoError(err)
			decoded, ok, err := Decode(CalDAVRawFormat, raw, meta, SourceContext{CalendarID: "example", CalendarTimeZone: "UTC"})
			requirements.NoError(err)
			requirements.True(ok)
			assertions.Equal(e.Event, decoded)
		}
	})
}

func TestCalDAVCaseInsensitiveParametersAndCivilDuration(t *testing.T) {
	for _, tc := range []struct{ name, dtstart, duration, params, end string }{
		{"civil day across DST", "DTSTART;TZID=America/New_York:20270313T090000", "P1D", "CUTYPE=room", "2027-03-14T13:00:00Z"},
		{"elapsed hours across DST", "DTSTART;TZID=America/New_York:20270313T090000", "PT24H", "ROLE=opt-participant", "2027-03-14T14:00:00Z"},
		{"lowercase DATE", "DTSTART;VALUE=date:20270314", "P1D", "CUTYPE=resource", "2027-03-15T04:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:case@example.org\r\n" + tc.dtstart + "\r\nDURATION:" + tc.duration + "\r\nATTENDEE;" + tc.params + ":mailto:room@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
			events, err := ParseCalDAVResource(raw, SourceContext{CalendarTimeZone: "America/New_York"})
			requirements.NoError(err)
			requirements.Len(events, 1)
			want, err := time.Parse(time.RFC3339, tc.end)
			requirements.NoError(err)
			assertions.Equal(want, *events[0].Event.End)
			role := "room"
			if strings.HasPrefix(tc.params, "ROLE=") {
				role = "optional"
			}
			assertions.Equal(role, events[0].Event.Attendees[0].Role)
		})
	}
}

func TestCalDAVPreservesGenericConferenceURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	joinURL := "https://video.example.org/conference/room/alpha"
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:conference@example.org\r\nDTSTART:20270314T130000Z\r\nCONFERENCE:" + joinURL + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	events, err := ParseCalDAVResource(raw, SourceContext{CalendarID: "calendar", CalendarTimeZone: "UTC"})
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal(joinURL, events[0].Event.JoinURL)
	assert.Empty(events[0].Event.JoinKey)
}
