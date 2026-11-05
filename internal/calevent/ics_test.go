package calevent

import (
	"os"
	"strings"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICSFloatingRecurrenceUsesSourceZone(t *testing.T) {
	for _, zone := range []string{"", "America/Chicago"} {
		t.Run(zone, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := strings.Replace(icsExample, "SUMMARY:", "RDATE:20270315T090000\r\nEXDATE:20270314T090000\r\nSUMMARY:", 1)
			components, err := ParseICS([]byte(raw), "", zone)
			requirements.NoError(err)
			e := components[0].Event
			loc := time.UTC
			if zone != "" {
				loc, err = time.LoadLocation(zone)
				requirements.NoError(err)
			}
			startZone, err := time.LoadLocation(e.EventTimeZone)
			requirements.NoError(err)
			set, err := calrecur.ParseSet(*e.Start, false, "", startZone, e.Recurrence)
			requirements.NoError(err)
			assertions.Equal(time.Date(2027, 3, 15, 9, 0, 0, 0, loc).UTC(), set.RDates[0].Instant)
			assertions.Equal(time.Date(2027, 3, 14, 9, 0, 0, 0, loc).UTC(), set.ExDates[0].Instant)
			raw = strings.Replace(raw, "SUMMARY:", "RECURRENCE-ID:20270315T090000\r\nSUMMARY:", 1)
			components, err = ParseICS([]byte(raw), "", zone)
			requirements.NoError(err)
			assertions.Equal(set.RDates[0].Instant, components[0].Event.Original.Instant)
		})
	}
}

func TestICSOutlookTZID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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

const icsExample = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:invite@example.com\r\nSEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART;TZID=America/New_York:20270314T090000\r\nDTEND;TZID=America/New_York:20270314T100000\r\nSUMMARY:Example\\, meeting\r\nDESCRIPTION:First line\\nSecond\r\n line\r\nORGANIZER:mailto:ORGANIZER@EXAMPLE.COM\r\nATTENDEE;CN=Guest;ROLE=OPT-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nATTENDEE;CUTYPE=ROOM:mailto:room@example.com\r\nLOCATION:https://meet.google.com/abc-defg-hij\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestICSDecode(t *testing.T) {
	for _, tt := range []struct{ name, raw, role string }{
		{"optional", icsExample, "optional"},
		{"chair", strings.Replace(icsExample, "ROLE=OPT-PARTICIPANT", "ROLE=CHAIR", 1), "required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			e, ok, err := Decode("ics", []byte(tt.raw), []byte(`{}`), "")
			requirements.NoError(err)
			requirements.True(ok)
			requirements.NotNil(e.Start)
			assertions.Equal("2027-03-14T13:00:00Z", e.Start.Format(time.RFC3339))
			requirements.Len(e.Attendees, 3)
			assertions.Equal("organizer", e.Attendees[2].Role)
			assertions.Equal("organizer@example.com", e.Attendees[2].Email)
			assertions.Equal(tt.role, e.Attendees[0].Role)
			assertions.Equal("accepted", e.Attendees[0].RSVP)
			assertions.Equal("room", e.Attendees[1].Role)
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

func TestICSRetainsEachEventWithItsTimeZones(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	components, err := ParseICS([]byte(icsExample), "", "")
	requirements.NoError(err)
	assertions.Equal([]byte(icsExample), components[0].Raw)

	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:first@example.test\r\nDTSTART;TZID=America/New_York:20270302T120000\r\nEND:VEVENT\r\n" +
		"BEGIN:VTODO\r\nUID:private-task@example.test\r\nSUMMARY:Private task\r\nEND:VTODO\r\n" +
		"BEGIN:VEVENT\r\nUID:second@example.test\r\nDTSTART:20270303T120000Z\r\nEND:VEVENT\r\n" +
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
		{"malformed property", "BEGIN:VCALENDAR\r\nSUMMARY;CN=x\r\nEND:VCALENDAR\r\n", "malformed iCalendar parameters"},
		{"second calendar", icsExample + icsExample, "calendar has trailing content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Decode("ics", []byte(tc.raw), []byte(`{}`), "")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestICSPeriodRecurrenceRemainsArchived(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	const period = "RDATE;VALUE=PERIOD:20270315T090000Z/20270315T100000Z"
	raw := strings.Replace(icsExample, "SUMMARY:", period+"\r\nSUMMARY:", 1)
	components, err := ParseICS([]byte(raw), "", "")
	requirements.NoError(err)
	requirements.Len(components, 1)
	e := components[0].Event
	assertions.Equal([]string{period}, e.Recurrence)
	_, err = calrecur.ParseSet(*e.Start, false, "", time.UTC, e.Recurrence)
	assertions.ErrorIs(err, calrecur.ErrUnsupportedRecurrence)
}
