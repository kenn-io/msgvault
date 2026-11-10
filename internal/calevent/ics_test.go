package calevent

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICSFloatingRecurrenceUsesSourceZone(t *testing.T) {
	for _, tc := range []struct{ zone, rdate, exdate, original string }{
		{"", "RDATE:20270315T090000Z", "EXDATE:20270314T090000Z", "2027-03-15T09:00:00Z"},
		{"America/Chicago", "RDATE:20270315T140000Z", "EXDATE:20270314T140000Z", "2027-03-15T14:00:00Z"},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := strings.Replace(icsExample, "SUMMARY:", "RDATE:20270315T090000\r\nEXDATE:20270314T090000\r\nSUMMARY:", 1)
			components, err := ParseICS([]byte(raw), "", tc.zone)
			requirements.NoError(err)
			assertions.Equal([]string{tc.rdate, tc.exdate}, components[0].Event.Recurrence)
			raw = strings.Replace(raw, "SUMMARY:", "RECURRENCE-ID:20270315T090000\r\nSUMMARY:", 1)
			components, err = ParseICS([]byte(raw), "", tc.zone)
			requirements.NoError(err)
			assertions.Equal(tc.original, components[0].Event.Original.Instant.Format(time.RFC3339))
		})
	}
	t.Run("period recurrence", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		const period = "RDATE;VALUE=PERIOD:20270315T090000Z/20270315T100000Z"
		raw := strings.Replace(icsExample, "SUMMARY:", period+"\r\nSUMMARY:", 1)
		components, err := ParseICS([]byte(raw), "", "")
		requirements.NoError(err)
		requirements.Len(components, 1)
		assertions.Equal([]string{period}, components[0].Event.Recurrence)
	})
}

func TestICSOutlookTZID(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	raw, err := os.ReadFile("testdata/outlook.ics")
	requirements.NoError(err)
	components, err := ParseICS(raw, "", "")
	requirements.NoError(err)
	requirements.Len(components, 1)
	e := components[0].Event
	assertions.Equal("2027-07-06T13:00:00Z", e.Start.Format(time.RFC3339))
	assertions.Equal("America/New_York", e.EventTimeZone)
	assertions.Equal([]string{"RDATE;TZID=America/New_York:20270707T090000"}, e.Recurrence)
}

func TestParseICSCustomizedTimeZone(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	raw, err := os.ReadFile("testdata/customized.ics")
	requirements.NoError(err)
	components, err := ParseICS(raw, "", "")
	requirements.NoError(err)
	requirements.Len(components, 1)
	assertions.Equal("2026-07-15T14:00:00Z", components[0].Event.Start.Format(time.RFC3339))
	assertions.Contains(components[0].Event.Recurrence, "EXDATE:20260722T140000Z")
	raw, err = os.ReadFile("testdata/customized-no-dst.ics")
	requirements.NoError(err)
	components, err = ParseICS(raw, "", "")
	requirements.NoError(err)
	requirements.Len(components, 1)
	assertions.Equal("2026-07-15T04:30:00Z", components[0].Event.Start.Format(time.RFC3339))
}

func TestICSLibicalTZID(t *testing.T) {
	for _, prefix := range []string{"/freeassociation.sourceforge.net/", "/freeassociation.sourceforge.net/Tzfile/"} {
		t.Run(prefix, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := strings.ReplaceAll(icsExample, "TZID=America/New_York", "TZID="+prefix+"America/New_York")
			components, err := ParseICS([]byte(raw), "", "")
			requirements.NoError(err)
			requirements.Len(components, 1)
			assertions.Equal("2027-03-14T13:00:00Z", components[0].Event.Start.Format(time.RFC3339))
			assertions.Equal("2027-03-14T14:00:00Z", components[0].Event.End.Format(time.RFC3339))
			assertions.Equal("America/New_York", components[0].Event.EventTimeZone)
		})
	}
}

const icsExample = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:invite@example.com\r\nSEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART;TZID=America/New_York:20270314T090000\r\nDTEND;TZID=America/New_York:20270314T100000\r\nSUMMARY:Example\\, meeting\r\nDESCRIPTION:First line\\nSecond\r\n line\r\nORGANIZER:mailto:ORGANIZER@EXAMPLE.COM\r\nATTENDEE;CN=Guest;ROLE=OPT-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nATTENDEE;CUTYPE=ROOM:mailto:room@example.com\r\nLOCATION:https://meet.google.com/abc-defg-hij\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestICSDecode(t *testing.T) {
	t.Run("non UTF-8 text", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		raw := strings.Replace(icsExample, "SUMMARY:Example\\, meeting", "SUMMARY:Caf\xe9 planning", 1)
		raw = strings.Replace(raw, "CN=Guest", "CN=Andr\xe9 Example", 1)
		components, err := ParseICS([]byte(raw), "", "")
		requirements.NoError(err)
		requirements.Len(components, 1)
		assertions.Equal("Café planning", components[0].Summary)
		requirements.NotEmpty(components[0].Event.Attendees)
		assertions.Equal("André Example", components[0].Event.Attendees[0].DisplayName)
	})
	assertions, requirements := assert.New(t), require.New(t)
	e, ok, err := Decode("ics", []byte(icsExample), []byte(`{}`), "")
	requirements.NoError(err)
	requirements.True(ok)
	requirements.NotNil(e.Start)
	assertions.Equal("2027-03-14T13:00:00Z", e.Start.Format(time.RFC3339))
	requirements.Len(e.Attendees, 3)
	assertions.Equal("organizer", e.Attendees[2].Role)
	assertions.Equal("organizer@example.com", e.Attendees[2].Email)
	assertions.Equal("room@example.com", e.Attendees[1].Email)
	assertions.Equal("accepted", e.Attendees[0].RSVP)
	assertions.Equal("room", e.Attendees[1].Role)
	for _, tt := range []struct{ name, raw, role string }{
		{"optional", icsExample, "optional"},
		{"chair", strings.Replace(icsExample, "ROLE=OPT-PARTICIPANT", "ROLE=CHAIR", 1), "required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			e, ok, err := Decode("ics", []byte(tt.raw), []byte(`{}`), "")
			requirements.NoError(err)
			requirements.True(ok)
			requirements.NotEmpty(e.Attendees)
			assertions.Equal(tt.role, e.Attendees[0].Role)
		})
	}
}

func TestICSNULFields(t *testing.T) {
	for _, tc := range []struct{ name, property, wantName, wantEmail, wantSummary string }{
		{"summary", "SUMMARY:Before\x00after", "", "", "Before\uFFFDafter"},
		{"attendee name", "ATTENDEE;CN=Before\x00after:mailto:guest@example.org", "Before\uFFFDafter", "guest@example.org", ""},
		{"organizer name", "ORGANIZER;CN=Before\x00after:mailto:organizer@example.com", "Before\uFFFDafter", "organizer@example.com", ""},
		{"address", "ATTENDEE:mailto:before\x00after@example.org", "", "before\uFFFDafter@example.org", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:nul@example.com\r\nDTSTART:20270302T120000Z\r\n" + tc.property + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", "")
			requirements.NoError(err)
			requirements.Len(components, 1)
			if tc.wantSummary != "" {
				assertions.Equal(tc.wantSummary, components[0].Summary)
				return
			}
			requirements.Len(components[0].Event.Attendees, 1)
			assertions.Equal(tc.wantName, components[0].Event.Attendees[0].DisplayName)
			assertions.Equal(tc.wantEmail, components[0].Event.Attendees[0].Email)
		})
	}
}

func TestICSMethods(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := strings.Replace(icsExample, "METHOD:REQUEST\r\n", "", 1)
	components, err := ParseICS([]byte(raw), "REPLY", "")
	requirements.NoError(err)
	assertions.Empty(components)
	components, err = ParseICS([]byte(raw), "CANCEL", "")
	requirements.NoError(err)
	requirements.Len(components, 1)
	assertions.Equal("cancelled", components[0].Event.Status)
}

func TestICSStatus(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"TENTATIVE", "tentative"},
		{"CANCELLED", "cancelled"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := strings.Replace(icsExample, "SUMMARY:", "STATUS:"+tc.status+"\r\nSUMMARY:", 1)
			components, err := ParseICS([]byte(raw), "", "")
			requirements.NoError(err)
			requirements.Len(components, 1)
			assertions.Equal(tc.want, components[0].Event.Status)
		})
	}
}

func TestICSRetainsEachEventWithItsTimeZones(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	components, err := ParseICS([]byte(icsExample), "", "")
	requirements.NoError(err)
	assertions.Equal([]byte(icsExample), components[0].Raw)

	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:first@example.test\r\nDTSTAMP:20270301T120000Z\r\nDTSTART;TZID=America/New_York:20270302T120000\r\nEND:VEVENT\r\n" +
		"BEGIN:VTODO\r\nUID:private-task@example.test\r\nSUMMARY:Private task\r\nEND:VTODO\r\n" +
		"BEGIN:VEVENT\r\nUID:second@example.test\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270303T120000Z\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
	components, err = ParseICS(raw, "", "UTC")
	requirements.NoError(err)
	requirements.Len(components, 2)
	for i, uid := range []string{"first@example.test", "second@example.test"} {
		retained := string(components[i].Raw)
		assertions.Contains(retained, "BEGIN:VTIMEZONE")
		assertions.NotContains(retained, "private-task@example.test")
		event, err := ParseICS(components[i].Raw, "", "UTC")
		requirements.NoError(err)
		requirements.Len(event, 1)
		assertions.Equal(uid, event[0].UID)
	}
}

func TestICSRecurrenceAndBounds(t *testing.T) {
	for _, tt := range []struct{ name, props, kind, end, original string }{
		{"all day default", "DTSTART;VALUE=DATE:20270314", "single", "2027-03-15T04:00:00Z", ""},
		{"civil duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D", "single", "2027-03-14T13:00:00Z", ""},
		{"elapsed duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:PT24H\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "2027-03-14T14:00:00Z", ""},
		{"sparse exception cancel", "RECURRENCE-ID;VALUE=DATE:20270314\r\nSTATUS:CANCELLED", "exception", "", "20270314"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:bounds@example.com\r\n" + tt.props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			e, ok, err := Decode("ics", []byte(raw), []byte(`{}`), "America/New_York")
			requirements.NoError(err)
			requirements.True(ok)
			assertions.Equal(tt.kind, e.Kind)
			if tt.end != "" {
				requirements.NotNil(e.End)
				assertions.Equal(tt.end, e.End.Format(time.RFC3339))
			}
			if tt.original != "" {
				requirements.NotNil(e.Original)
				assertions.Equal(tt.original, e.Original.Key())
			}
		})
	}
}

func TestICSRejectMalformed(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"unknown method", strings.Replace(icsExample, "METHOD:REQUEST", "METHOD:X-UNKNOWN", 1), `unsupported calendar method "X-UNKNOWN"`},
		{"unknown zone", strings.ReplaceAll(icsExample, "America/New_York", "Unknown/Zone"), "unknown time zone Unknown/Zone"},
		{"negative sequence", strings.Replace(icsExample, "SEQUENCE:2", "SEQUENCE:-1", 1), "invalid SEQUENCE"},
		{"missing UID", strings.Replace(icsExample, "UID:invite@example.com\r\n", "", 1), "event has no UID"},
		{"ends after year 9999", strings.NewReplacer("DTSTART;TZID=America/New_York:20270314T090000", "DTSTART:99991231T230000Z", "DTEND;TZID=America/New_York:20270314T100000", "DURATION:PT2H").Replace(icsExample), "outside years 0 to 9999"},
		{"elapsed DURATION overflow", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DURATION:PT18446744074S", 1), "calendar DURATION exceeds limit"},
		{"week DURATION overflow", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DURATION:P1317624576693539403W", 1), "calendar DURATION exceeds limit"},
		{"hour DURATION overflow", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DURATION:PT5124096H", 1), "calendar DURATION exceeds limit"},
		{"combined DURATION overflow", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DURATION:PT2562047H47M17S", 1), "calendar DURATION exceeds limit"},
		{"end and duration", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DTEND;TZID=America/New_York:20270314T100000\r\nDURATION:PT1H", 1), "event has both DTEND and DURATION"},
		{"timed start and date end", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DTEND;VALUE=DATE:20270315", 1), "DTEND value type differs from DTSTART"},
		{"date start and timed end", strings.Replace(icsExample, "DTSTART;TZID=America/New_York:20270314T090000", "DTSTART;VALUE=DATE:20270314", 1), "DTEND value type differs from DTSTART"},
		{"timed start and zero date end", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DTEND;VALUE=DATE:00010101", 1), "DTEND value type differs from DTSTART"},
		{"date start and zero timed end", strings.NewReplacer("DTSTART;TZID=America/New_York:20270314T090000", "DTSTART;VALUE=DATE:20270314", "DTEND;TZID=America/New_York:20270314T100000", "DTEND:00010101T000000Z").Replace(icsExample), "DTEND value type differs from DTSTART"},
		{"malformed property", "BEGIN:VCALENDAR\r\nSUMMARY;CN=x\r\nEND:VCALENDAR\r\n", "malformed iCalendar parameters"},
		{"second calendar", icsExample + icsExample, "calendar has trailing content"},
		{"ranged recurrence", strings.Replace(icsExample, "SUMMARY:", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270314T140000Z\r\nSUMMARY:", 1), "RECURRENCE-ID: RANGE is unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Decode("ics", []byte(tc.raw), []byte(`{}`), "")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestICSLargeElapsedDuration(t *testing.T) {
	raw := strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DURATION:PT3000000000S", 1)
	components, err := ParseICS([]byte(raw), "", "")
	require.NoError(t, err)
	require.Len(t, components, 1)
	assert.Equal(t, 3000000000*time.Second, components[0].Event.End.Sub(*components[0].Event.Start))
}

func TestParseICSCustomizedHistory(t *testing.T) {
	raw, err := os.ReadFile("testdata/customized-history.ics")
	require.NoError(t, err)
	for _, until := range []bool{false, true} {
		data := string(raw)
		if until {
			data = strings.Replace(data, "BYDAY=-1SU;BYMONTH=10", "BYDAY=-1SU;BYMONTH=10;UNTIL=20061029T060000Z", 1)
			data = strings.Replace(data, "BYDAY=1SU;BYMONTH=4", "BYDAY=1SU;BYMONTH=4;UNTIL=20060402T070000Z", 1)
		}
		for _, tc := range []struct{ date, want string }{{"20060320", "2006-03-20T15:00:00Z"}, {"20061101", "2006-11-01T15:00:00Z"}, {"20260320", "2026-03-20T14:00:00Z"}} {
			t.Run(tc.date+fmt.Sprint(until), func(t *testing.T) {
				components, err := ParseICS([]byte(strings.Replace(data, "20060320T100000", tc.date+"T100000", 1)), "", "")
				require.NoError(t, err)
				require.Len(t, components, 1)
				assert.Equal(t, tc.want, components[0].Event.Start.Format(time.RFC3339))
				assert.Contains(t, components[0].Event.Recurrence, "EXDATE:20080320T140000Z")
				loc, err := LoadZone(components[0].Event.EventTimeZone)
				require.NoError(t, err)
				date, err := time.Parse("20060102", tc.date)
				require.NoError(t, err)
				assert.Equal(t, tc.want, time.Date(date.Year(), date.Month(), date.Day(), 10, 0, 0, 0, loc).UTC().Format(time.RFC3339))
			})
		}
	}
	_, err = LoadZone("STD+05:00;bad")
	require.Error(t, err)
}

func TestParseICSOneShotZoneChange(t *testing.T) {
	raw, err := os.ReadFile("testdata/customized-history.ics")
	require.NoError(t, err)
	change := "BEGIN:STANDARD\nDTSTART:20300101T020000\nTZOFFSETFROM:-0500\nTZOFFSETTO:-0600\nEND:STANDARD\n"
	data := strings.Replace(string(raw), "END:VTIMEZONE", change+"END:VTIMEZONE", 1)
	components, err := ParseICS([]byte(data), "", "")
	require.NoError(t, err)
	loc, err := LoadZone(components[0].Event.EventTimeZone)
	require.NoError(t, err)
	for _, tc := range []struct {
		year int
		want int
	}{{2006, -5 * 3600}, {2026, -4 * 3600}, {2031, -6 * 3600}} {
		_, got := time.Date(tc.year, 3, 20, 10, 0, 0, 0, loc).Zone()
		assert.Equal(t, tc.want, got)
	}
}
