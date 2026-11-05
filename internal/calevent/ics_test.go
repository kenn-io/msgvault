package calevent

import (
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
}

func TestICSOutlookTZID(t *testing.T) {
	raw, err := os.ReadFile("testdata/outlook.ics")
	require.NoError(t, err)
	for _, tc := range []struct{ windows, zone, start string }{
		{"Eastern Standard Time", "America/New_York", "2027-07-06T13:00:00Z"},
		{"British Columbia Standard Time", "America/Vancouver", "2027-07-06T16:00:00Z"},
		{"Alberta Standard Time", "America/Edmonton", "2027-07-06T15:00:00Z"},
		{"Manitoba Standard Time", "America/Winnipeg", "2027-07-06T14:00:00Z"},
	} {
		t.Run(tc.windows, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			input := string(raw)
			if tc.windows != "Eastern Standard Time" {
				start := strings.Index(input, "BEGIN:VTIMEZONE")
				end := strings.Index(input, "END:VTIMEZONE\n") + len("END:VTIMEZONE\n")
				input = strings.ReplaceAll(input[:start]+input[end:], "Eastern Standard Time", tc.windows)
			}
			components, err := ParseICS([]byte(input), "", "")
			requirements.NoError(err)
			requirements.Len(components, 1)
			e := components[0].Event
			assertions.Equal(tc.start, e.Start.Format(time.RFC3339))
			assertions.Equal(tc.zone, e.EventTimeZone)
			assertions.Equal([]string{"RDATE;TZID=" + tc.zone + ":20270707T090000"}, e.Recurrence)
		})
	}
}

func TestParseICSCustomizedTimeZone(t *testing.T) {
	raw, err := os.ReadFile("testdata/customized.ics")
	require.NoError(t, err)
	t.Run("weekly invite", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		components, err := ParseICS(raw, "", "")
		requirements.NoError(err)
		requirements.Len(components, 1)
		e := components[0].Event
		assertions.Equal("2026-07-15T14:00:00Z", e.Start.Format(time.RFC3339))
		assertions.Contains(e.Recurrence, "EXDATE:20260722T140000Z")
	})
	t.Run("missing offset", func(t *testing.T) {
		_, err := ParseICS([]byte(strings.Replace(string(raw), "TZOFFSETTO:-0500\n", "", 1)), "", "")
		require.Error(t, err)
	})
	noDST, err := os.ReadFile("testdata/customized-no-dst.ics")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, raw string
		wantError bool
	}{
		{"without daylight saving", string(noDST), false},
		{"one-time offset change", strings.Replace(strings.Replace(string(noDST), "BEGIN:DAYLIGHT\nDTSTART:16010101T000000\nTZOFFSETFROM:+0530\nTZOFFSETTO:+0530\nEND:DAYLIGHT\n", "", 1), "TZOFFSETFROM:+0530", "TZOFFSETFROM:+0430", 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			components, err := ParseICS([]byte(tc.raw), "", "")
			if tc.wantError {
				requirements.ErrorContains(err, "unsupported VTIMEZONE")
				return
			}
			requirements.NoError(err)
			requirements.Len(components, 1)
			requirements.Equal("2026-07-15T04:30:00Z", components[0].Event.Start.Format(time.RFC3339))
		})
	}
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
			assertions.Equal("room@example.com", e.Attendees[1].Email)
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

func TestICSStatus(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"", "confirmed"},
		{"CONFIRMED", "confirmed"},
		{"TENTATIVE", "tentative"},
		{"CANCELLED", "cancelled"},
		{"COMPLETED", ""},
	} {
		t.Run(tc.status, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := strings.Replace(icsExample, "SUMMARY:", "STATUS:"+tc.status+"\r\nSUMMARY:", 1)
			components, err := ParseICS([]byte(raw), "", "")
			if tc.want == "" {
				requirements.ErrorContains(err, "invalid VEVENT STATUS")
				return
			}
			requirements.NoError(err)
			requirements.Len(components, 1)
			assertions.Equal(tc.want, components[0].Event.Status)
		})
	}
}

func TestICSDurationLimit(t *testing.T) {
	for _, tc := range []struct {
		duration string
		withEnd  bool
		want     string
		wantEnd  string
	}{
		{"PT6000000H", false, "calendar DURATION exceeds limit", ""},
		{"P36600DT1S", false, "calendar DURATION exceeds limit", ""},
		{"P5229W", false, "calendar DURATION exceeds limit", ""},
		{"garbage", true, "invalid calendar DURATION", ""},
		{"P", false, "invalid calendar DURATION", ""},
		{"PT", false, "invalid calendar DURATION", ""},
		{"P1DT", false, "invalid calendar DURATION", ""},
		{"PT1H", false, "", "2027-03-14T14:00:00Z"},
		{"P1D", false, "", "2027-03-15T13:00:00Z"},
	} {
		name := tc.duration
		if tc.withEnd {
			name += " with DTEND"
		}
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DURATION:"+tc.duration, 1)
			if tc.withEnd {
				raw = strings.Replace(icsExample, "SUMMARY:", "DURATION:"+tc.duration+"\r\nSUMMARY:", 1)
			}
			components, err := ParseICS([]byte(raw), "", "")
			if tc.want != "" {
				requirements.ErrorContains(err, tc.want)
				return
			}
			requirements.NoError(err)
			requirements.Len(components, 1)
			requirements.NotNil(components[0].Event.End)
			assertions.Equal(tc.wantEnd, components[0].Event.End.Format(time.RFC3339))
		})
	}
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
		{"date start with timed end", strings.NewReplacer("DTSTART;TZID=America/New_York:20270314T090000", "DTSTART;VALUE=DATE:20270314", "DTEND;TZID=America/New_York:20270314T100000", "DTEND:20270315T180000Z").Replace(icsExample), "event start and end have different value types"},
		{"timed start with date end", strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DTEND;VALUE=DATE:20270315", 1), "event start and end have different value types"},
		{"all day duration with hours", strings.NewReplacer("DTSTART;TZID=America/New_York:20270314T090000", "DTSTART;VALUE=DATE:20270314", "DTEND;TZID=America/New_York:20270314T100000", "DURATION:P1DT2H").Replace(icsExample), "all-day event duration contains a time component"},
		{"all day duration with minutes", strings.NewReplacer("DTSTART;TZID=America/New_York:20270314T090000", "DTSTART;VALUE=DATE:20270314", "DTEND;TZID=America/New_York:20270314T100000", "DURATION:P1DT2M").Replace(icsExample), "all-day event duration contains a time component"},
		{"all day duration with seconds", strings.NewReplacer("DTSTART;TZID=America/New_York:20270314T090000", "DTSTART;VALUE=DATE:20270314", "DTEND;TZID=America/New_York:20270314T100000", "DURATION:P1DT2S").Replace(icsExample), "all-day event duration contains a time component"},
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
}
