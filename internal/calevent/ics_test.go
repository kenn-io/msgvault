package calevent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICSLocalTimesUseRFC5545GapAndFold(t *testing.T) {
	for _, tt := range []struct{ name, prop, zone, want string }{
		{"Berlin TZID fold", "DTSTART;TZID=Europe/Berlin:20271031T023000", "UTC", "2027-10-31T00:30:00Z"},
		{"Berlin floating fold", "DTSTART:20271031T023000", "Europe/Berlin", "2027-10-31T00:30:00Z"},
		{"New York TZID gap", "DTSTART;TZID=America/New_York:20270314T023000", "UTC", "2027-03-14T07:30:00Z"},
		{"New York floating gap", "DTSTART:20270314T023000", "America/New_York", "2027-03-14T07:30:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:dst@example.test\r\n" + tt.prop + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", tt.zone)
			requirements.NoError(err)
			requirements.Len(components, 1)
			requirements.NotNil(components[0].Event.Start)
			assertions.Equal(tt.want, components[0].Event.Start.Format(time.RFC3339))
		})
	}
}

func TestICSAllDaySkippedDateBounds(t *testing.T) {
	for _, tt := range []struct {
		name, props string
		wantError   bool
	}{
		{"explicit end", "DTSTART;VALUE=DATE:20111230\r\nDTEND;VALUE=DATE:20111231", false},
		{"default end", "DTSTART;VALUE=DATE:20111230", false},
		{"duration", "DTSTART;VALUE=DATE:20111230\r\nDURATION:P1D", false},
		{"reversed dates", "DTSTART;VALUE=DATE:20111231\r\nDTEND;VALUE=DATE:20111230", true},
		{"equal dates", "DTSTART;VALUE=DATE:20111230\r\nDTEND;VALUE=DATE:20111230", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:skipped-date@example.test\r\n" + tt.props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", "Pacific/Apia")
			if tt.wantError {
				requirements.ErrorContains(err, "event ends before its start")
				return
			}
			requirements.NoError(err)
			requirements.Len(components, 1)
			event := components[0].Event
			assertions.True(event.AllDay)
			assertions.Equal("2011-12-30", event.StartDate)
			assertions.Equal("2011-12-31", event.EndDate)
			requirements.NotNil(event.Start)
			requirements.NotNil(event.End)
			assertions.Equal("2011-12-30T10:00:00Z", event.Start.Format(time.RFC3339))
			assertions.Equal("2011-12-30T10:00:00Z", event.End.Format(time.RFC3339))
		})
	}
}

func TestICSRejectsHostLocalTimeZone(t *testing.T) {
	for _, tc := range []struct{ name, prop, zone string }{
		{"TZID", "DTSTART;TZID=Local:20270302T120000", "UTC"},
		{"source zone", "DTSTART:20270302T120000", "Local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:local@example.test\r\n" + tc.prop + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
			components, _, err := ParseICSWithDiagnostics(t.Context(), raw, "", tc.zone, NewICSParseBudget())
			require.ErrorContains(t, err, "unsupported time zone")
			assert.Empty(t, components)
		})
	}
}

func TestICSMIMEMethodFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := strings.Replace(icsExample, "METHOD:REQUEST\r\n", "", 1)
	components, err := ParseICS([]byte(raw), "REPLY", "")
	requirements.NoError(err)
	assertions.Empty(components)
	_, err = ParseICS([]byte(icsExample), "REPLY", "")
	requirements.ErrorContains(err, "conflicts")
}

func TestICSCaseInsensitiveValueParameters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, value := range []string{"date", "DaTe"} {
		raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:date@example.test\r\nDTSTART;VALUE=" + value + ":20270314\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		components, err := ParseICS([]byte(raw), "", "")
		requirements.NoError(err)
		requirements.Len(components, 1)
		assertions.True(components[0].Event.AllDay)
		assertions.Equal("2027-03-14", components[0].Event.StartDate)
	}
}

func TestICSLargeExport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var raw strings.Builder
	raw.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nEND:VTIMEZONE\r\n")
	for i := range 10000 {
		fmt.Fprintf(&raw, "BEGIN:VEVENT\r\nUID:export-%d@example.test\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Event %d\r\nDESCRIPTION:%s\r\nEND:VEVENT\r\n", i, i, strings.Repeat("x", 420))
	}
	raw.WriteString("END:VCALENDAR\r\n")
	requirements.Greater(raw.Len(), 4<<20)
	components, err := ParseICS([]byte(raw.String()), "", "")
	requirements.NoError(err)
	requirements.Len(components, 10000)
	for _, i := range []int{0, 4999, 9999} {
		retained, err := ParseICS(components[i].Raw, "", "")
		requirements.NoError(err)
		requirements.Len(retained, 1)
		assertions.Equal(fmt.Sprintf("export-%d@example.test", i), retained[0].UID)
		assertions.Contains(string(components[i].Raw), "BEGIN:VTIMEZONE")
	}
}

func TestICSRetainedComponentsHaveCumulativeLimit(t *testing.T) {
	requirements := require.New(t)
	var raw strings.Builder
	raw.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	shared := strings.Repeat("x", 7800)
	for i := range 80 {
		fmt.Fprintf(&raw, "X-SHARED-%02d:%s\r\n", i, shared)
	}
	for i := range 60 {
		fmt.Fprintf(&raw, "BEGIN:VEVENT\r\nUID:bounded-%d@example.test\r\nDTSTART:20270302T120000Z\r\nEND:VEVENT\r\n", i)
	}
	raw.WriteString("END:VCALENDAR\r\n")
	requirements.Less(raw.Len(), MaxICSBytes)
	components, err := ParseICS([]byte(raw.String()), "", "")
	requirements.ErrorContains(err, "retained event data exceeds 32 MiB limit")
	requirements.Empty(components)
}

func TestICSRetainsOriginalWallDTStartThroughGap(t *testing.T) {
	requirements := require.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:gap@example.test\r\n" +
		"DTSTART;TZID=America/New_York:20270314T023000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	components, err := ParseICS(raw, "", "UTC")
	requirements.NoError(err)
	requirements.Len(components, 1)
	requirements.NotNil(components[0].Event.StartWall)
	assert.Equal(t, "2027-03-14T02:30:00Z", components[0].Event.StartWall.Format(time.RFC3339))
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

			e, ok, err := Decode("ics", []byte(tt.raw), []byte(`{"ics_version":1}`), "")
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

func TestICSComponentRawDropsNonEventSiblings(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:selected-event@example.test\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Selected event\r\nEND:VEVENT\r\n" +
		"BEGIN:VTODO\r\nUID:private-task@example.test\r\nSUMMARY:Private task\r\nDESCRIPTION:personal reminder\r\nEND:VTODO\r\n" +
		"BEGIN:VJOURNAL\r\nUID:private-journal@example.test\r\nSUMMARY:Private journal\r\nEND:VJOURNAL\r\n" +
		"END:VCALENDAR\r\n")
	components, err := ParseICS(raw, "REQUEST", "UTC")
	requirements.NoError(err)
	requirements.Len(components, 1)
	filtered := components[0].Raw
	assertions.Contains(string(filtered), "UID:selected-event@example.test")
	assertions.NotContains(string(filtered), "private-task@example.test")
	assertions.NotContains(string(filtered), "private-journal@example.test")
	event, err := ParseICS(filtered, "REQUEST", "UTC")
	requirements.NoError(err)
	assertions.Equal("selected-event@example.test", event[0].UID)
}

func TestICSRecurrenceAndBounds(t *testing.T) {
	for _, tt := range []struct {
		name, props, kind, end, original string
		unsupported                      bool
	}{
		{"all day default", "DTSTART;VALUE=DATE:20270314", "single", "2027-03-15T04:00:00Z", "", false},
		{"civil duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D", "single", "2027-03-14T13:00:00Z", "", false},
		{"fixed recurring duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:PT24H\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "2027-03-14T14:00:00Z", "", false},
		{"elapsed duration in repeated hour", "DTSTART;TZID=Europe/Berlin:20271031T023000\r\nDURATION:PT1H", "single", "2027-10-31T01:30:00Z", "", false},
		{"recurring elapsed duration in repeated hour", "DTSTART;TZID=Europe/Berlin:20271031T023000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "2027-10-31T01:30:00Z", "", false},
		{"sparse exception cancel", "RECURRENCE-ID;VALUE=DATE:20270314\r\nSTATUS:CANCELLED", "exception", "", "20270314", false},
		{"mixed bounds", "DTSTART;TZID=America/New_York:20270308T090000\r\nDTEND:20270308T100000", "single", "2027-03-08T15:00:00Z", "", false},
		{"nominal day", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "", "", true},
		{"nominal week", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1W\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "", "", true},
		{"all day recurring duration", "DTSTART;VALUE=DATE:20270313\r\nDURATION:P1D\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "", "", false},
		{"UTC nominal duration", "DTSTART:20270313T090000Z\r\nDURATION:P1D\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:bounds@example.com\r\n" + tt.props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			e, ok, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1}`), "America/New_York")
			requirements.NoError(err)
			requirements.True(ok)
			assertions.Equal(tt.kind, e.Kind)
			if tt.unsupported {
				assertions.NotEmpty(e.RecurrenceError)
			} else {
				assertions.Empty(e.RecurrenceError)
			}
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
	for _, raw := range []string{
		strings.Replace(icsExample, "METHOD:REQUEST", "METHOD:X-UNKNOWN", 1),
		strings.Replace(icsExample, "UID:invite@example.com", "UID:invite@example.com\r\nUID:other@example.com", 1),
		strings.ReplaceAll(icsExample, "America/New_York", "Unknown/Zone"),
		strings.Replace(icsExample, "SEQUENCE:2", "SEQUENCE:-1", 1),
		strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DTEND;VALUE=DATE:20270315", 1),
		strings.Replace(icsExample, "SUMMARY:Example\\, meeting", "SUMMARY;CN=\"unterminated:Example", 1),
		icsExample + icsExample,
		strings.Replace(icsExample, "invite@example.com", "invalid\xff@example.com", 1),
		strings.Replace(icsExample, "SUMMARY:Example\\, meeting", "SUMMARY:"+strings.Repeat("x", 8193), 1),
	} {
		_, _, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1}`), "")
		require.Error(t, err)
	}
	_, _, err := Decode("ics", []byte(icsExample), nil, "")
	require.Error(t, err)
	_, _, err = Decode("ics", []byte(icsExample), []byte(`{"ics_version":2}`), "")
	require.ErrorContains(t, err, "2")
}

func FuzzICSDecode(f *testing.F) {
	f.Add([]byte(icsExample))
	f.Add([]byte("BEGIN:VCALENDAR\r\nSUMMARY;CN=x\r\nEND:VCALENDAR\r\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _, _ = Decode("ics", raw, []byte(`{"ics_version":1}`), "")
	})
}

func TestICSRejectControlsAndUnrepresentableBounds(t *testing.T) {
	for _, props := range []string{
		"UID:invalid\x00@example.com\r\nDTSTART:20270302T120000Z",
		"UID:overflow@example.com\r\nDTSTART;VALUE=DATE:99991231",
		"UID:overflow@example.com\r\nDTSTART:99991231T120000Z\r\nDURATION:P1D",
	} {
		raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n" + props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		_, _, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1}`), "")
		assert.Error(t, err)
	}
}

func TestICSFoldedComponentLimits(t *testing.T) {
	event := "BEGIN:VE\r\n VENT\r\nUID:folded@example.com\r\nDTSTART:20270302T120000Z\r\nEND:VE\r\n VENT\r\n"
	t.Run("valid", func(t *testing.T) {
		components, err := ParseICS([]byte("BEGIN:VCALENDAR\r\n"+event+"END:VCALENDAR\r\n"), "", "")
		require.NoError(t, err)
		require.Len(t, components, 1)
		assert.Equal(t, "folded@example.com", components[0].UID)
	})
	t.Run("event limit", func(t *testing.T) {
		_, err := ParseICS([]byte("BEGIN:VCALENDAR\r\n"+strings.Repeat(event, 10001)+"END:VCALENDAR\r\n"), "", "")
		require.ErrorContains(t, err, "10000 VEVENT")
	})
	t.Run("nesting", func(t *testing.T) {
		// Split property names to check the logical-line nesting guard before
		// the dependency's recursive component parser runs.
		nested := "BEGIN:VCALENDAR\r\n" + strings.Repeat("BE\r\n GIN:X\r\n", 33) + strings.Repeat("EN\r\n D:X\r\n", 33) + "END:VCALENDAR\r\n"
		_, err := ParseICS([]byte(nested), "", "")
		require.EqualError(t, err, "calendar exceeds nesting limit")
	})
}

func TestICSTextErrors(t *testing.T) {
	for _, name := range []string{"SUMMARY", "DESCRIPTION", "LOCATION", "UID", "STATUS", "METHOD"} {
		t.Run(name, func(t *testing.T) {
			raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:example\r\nDTSTART:20270302T120000Z\r\n" + name + ":broken\\q\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			if name == "UID" {
				raw = strings.Replace(raw, "UID:example\r\n", "", 1)
			}
			if name == "METHOD" {
				raw = strings.Replace(icsExample, "METHOD:REQUEST", "METHOD:broken\\q", 1)
			}
			_, err := ParseICS([]byte(raw), "", "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), name)
		})
	}
}

func TestICSTimeZoneUsesApplicableObservance(t *testing.T) {
	for _, order := range []bool{false, true} {
		t.Run(strconv.FormatBool(order), func(t *testing.T) {
			old := "BEGIN:STANDARD\r\nDTSTART:20110327T020000\r\nTZOFFSETFROM:+0300\r\nTZOFFSETTO:+0400\r\nEND:STANDARD\r\n"
			current := "BEGIN:STANDARD\r\nDTSTART:20141026T020000\r\nTZOFFSETFROM:+0400\r\nTZOFFSETTO:+0300\r\nEND:STANDARD\r\n"
			observances := old + current
			if order {
				observances = current + old
			}
			raw := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:Europe/Moscow\r\n" + observances + "END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:moscow@example.com\r\nDTSTART;TZID=Europe/Moscow:20270302T120000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", "UTC")
			require.NoError(t, err)
			require.Len(t, components, 1)
			assert.Equal(t, "2027-03-02T09:00:00Z", components[0].Event.Start.Format(time.RFC3339))
		})
	}
}
