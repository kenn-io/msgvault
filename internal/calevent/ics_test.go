package calevent

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const icsExample = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:invite@example.com\r\nSEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART;TZID=America/New_York:20270314T090000\r\nDTEND;TZID=America/New_York:20270314T100000\r\nSUMMARY:Example\\, meeting\r\nDESCRIPTION:First line\\nSecond\r\n line\r\nORGANIZER:mailto:ORGANIZER@EXAMPLE.COM\r\nATTENDEE;CN=Guest;ROLE=OPT-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nATTENDEE;CUTYPE=ROOM:mailto:room@example.com\r\nLOCATION:https://meet.google.com/abc-defg-hij\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestICSDecode(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	e, ok, err := Decode("ics", []byte(icsExample), []byte(`{"ics_version":1,"ics_event_index":0,"ics_namespace":"example"}`), SourceContext{})
	requirements.NoError(err)
	requirements.True(ok)
	assertions.Equal("ics", e.Provider)
	assertions.Equal("example", e.Namespace)
	assertions.Equal("invite@example.com", e.ICalUID)
	assertions.Equal(int64(2), e.Sequence)
	requirements.NotNil(e.Start)
	assertions.Equal("2027-03-14T13:00:00Z", e.Start.Format(time.RFC3339))
	assertions.Equal("First line\nSecondline", e.Description)
	assertions.Equal("organizer@example.com", e.OrganizerEmail)
	assertions.Equal("meet:abc-defg-hij", e.JoinKey)
	requirements.Len(e.Attendees, 2)
	assertions.Equal("optional", e.Attendees[0].Role)
	assertions.Equal("accepted", e.Attendees[0].RSVP)
	assertions.Equal("room", e.Attendees[1].Role)
}

func TestICSDecodeParsesOnlySelectedEventInItsRetainedTimeZone(t *testing.T) {
	requirements := require.New(t)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:retained-date@example.test\r\nDTSTART;VALUE=DATE:20111230\r\nDTEND;VALUE=DATE:20111231\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:new-event@example.test\r\nDTSTART:20120102T090000\r\nDTEND:20120102T100000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	metadata := []byte(`{"ics_version":1,"ics_event_index":1,"ics_time_zone":"Pacific/Apia"}`)
	event, ok, err := Decode("ics", []byte(raw), metadata, SourceContext{})
	requirements.NoError(err)
	requirements.True(ok)
	requirements.Equal("new-event@example.test", event.ICalUID)
	requirements.Equal("Pacific/Apia", event.EventTimeZone)
}

func TestICSRecurrenceAndBounds(t *testing.T) {
	for _, tt := range []struct{ name, props, kind, end, original, recurError string }{
		{"all day default", "DTSTART;VALUE=DATE:20270314", "single", "2027-03-15T04:00:00Z", "", ""},
		{"civil duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D", "single", "2027-03-14T13:00:00Z", "", ""},
		{"fixed recurring duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:PT24H\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "2027-03-14T14:00:00Z", "", ""},
		{"nominal recurring duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D\r\nRRULE:FREQ=DAILY;COUNT=2", "series", "2027-03-14T13:00:00Z", "", "nominal"},
		{"excluded recurrence is unexpandable", "DTSTART:20270301T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nEXRULE:FREQ=DAILY;COUNT=1", "series", "", "", "EXRULE"},
		{"sparse exception cancel", "RECURRENCE-ID;VALUE=DATE:20270314\r\nSTATUS:CANCELLED", "exception", "", "20270314", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:bounds@example.com\r\n" + tt.props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			e, ok, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1,"ics_event_index":0,"ics_time_zone":"America/New_York"}`), SourceContext{})
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
			if tt.recurError != "" {
				assertions.Contains(e.RecurrenceError, tt.recurError)
			}
		})
	}
}

func TestICSAllDayDatesPreserveCivilDateAcrossMidnightGap(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:date-gap@example.test\r\nDTSTART;VALUE=DATE:20181104\r\nDTEND;VALUE=DATE:20181105\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	metadata := []byte(`{"ics_version":1,"ics_event_index":0,"ics_time_zone":"America/Sao_Paulo"}`)
	event, ok, err := Decode("ics", []byte(raw), metadata, SourceContext{})
	requirements.NoError(err)
	requirements.True(ok)
	assertions.Equal("2018-11-04", event.StartDate)
	assertions.Equal("2018-11-05", event.EndDate)
	requirements.NotNil(event.Start)
	requirements.NotNil(event.End)
	assertions.Equal("2018-11-04T03:00:00Z", event.Start.Format(time.RFC3339))
	assertions.Equal("2018-11-05T02:00:00Z", event.End.Format(time.RFC3339))

	child := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:date-gap@example.test\r\nRECURRENCE-ID;VALUE=DATE:20181104\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	component, ok, err := Decode("ics", []byte(child), metadata, SourceContext{})
	requirements.NoError(err)
	requirements.True(ok)
	requirements.NotNil(component.Original)
	assertions.Equal("20181104", component.Original.Key())
}

func TestICSAllDayDurationUsesCivilDaysAcrossMidnightGap(t *testing.T) {
	for _, tt := range []struct {
		name, props, startDate, endDate, start, end string
	}{
		{"default end", "DTSTART;VALUE=DATE:20181104", "2018-11-04", "2018-11-05", "2018-11-04T03:00:00Z", "2018-11-05T02:00:00Z"},
		{"duration", "DTSTART;VALUE=DATE:20181103\r\nDURATION:P1D", "2018-11-03", "2018-11-04", "2018-11-03T03:00:00Z", "2018-11-04T03:00:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:date-duration@example.test\r\n" + tt.props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			metadata := []byte(`{"ics_version":1,"ics_event_index":0,"ics_time_zone":"America/Sao_Paulo"}`)
			event, ok, err := Decode("ics", []byte(raw), metadata, SourceContext{})
			requirements.NoError(err)
			requirements.True(ok)
			assertions.Equal(tt.startDate, event.StartDate)
			assertions.Equal(tt.endDate, event.EndDate)
			requirements.NotNil(event.Start)
			requirements.NotNil(event.End)
			assertions.Equal(tt.start, event.Start.Format(time.RFC3339))
			assertions.Equal(tt.end, event.End.Format(time.RFC3339))
		})
	}
}

func TestICSRejectMalformed(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(icsExample, "UID:invite@example.com", "UID:invite@example.com\r\nUID:other@example.com", 1),
		strings.ReplaceAll(icsExample, "America/New_York", "Unknown/Zone"),
		strings.Replace(icsExample, "SEQUENCE:2", "SEQUENCE:-1", 1),
		strings.Replace(icsExample, "DTEND;TZID=America/New_York:20270314T100000", "DTEND;VALUE=DATE:20270315", 1),
		strings.Replace(icsExample, "SUMMARY:Example\\, meeting", "SUMMARY;CN=\"unterminated:Example", 1),
		icsExample + icsExample,
		strings.Repeat("BEGIN:X\r\n", 33) + strings.Repeat("END:X\r\n", 33),
		strings.Replace(icsExample, "SUMMARY:Example\\, meeting", "SUMMARY:"+strings.Repeat("x", 8193), 1),
	} {
		_, _, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1,"ics_event_index":0}`), SourceContext{})
		require.Error(t, err)
	}
	_, _, err := Decode("ics", []byte(icsExample), nil, SourceContext{})
	require.Error(t, err)
	_, _, err = Decode("ics", []byte(icsExample), []byte(`{"ics_version":1,"ics_event_index":9}`), SourceContext{})
	require.Error(t, err)
}

func FuzzICSDecode(f *testing.F) {
	f.Add([]byte(icsExample))
	f.Add([]byte("BEGIN:VCALENDAR\r\nSUMMARY;CN=x\r\nEND:VCALENDAR\r\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _, _ = Decode("ics", raw, []byte(`{"ics_version":1,"ics_event_index":0}`), SourceContext{})
	})
}

func TestICSRejectInvalidUTF8(t *testing.T) {
	raw := strings.Replace(icsExample, "invite@example.com", "invalid\xff@example.com", 1)
	_, _, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1,"ics_event_index":0}`), SourceContext{})
	require.Error(t, err)
}

func TestICSRejectControlsAndUnrepresentableBounds(t *testing.T) {
	for _, props := range []string{
		"UID:invalid\x00@example.com\r\nDTSTART:20270302T120000Z",
		"UID:overflow@example.com\r\nDTSTART;VALUE=DATE:99991231",
		"UID:overflow@example.com\r\nDTSTART:99991231T120000Z\r\nDURATION:P1D",
	} {
		raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n" + props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		_, _, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1,"ics_event_index":0}`), SourceContext{})
		assert.Error(t, err)
	}
}

func TestICSRejectParameterizedComponentFraming(t *testing.T) {
	for _, raw := range []string{
		strings.ReplaceAll(strings.ReplaceAll(icsExample, "BEGIN:VEVENT", "BEGIN;X=1:"+"VEVENT"), "END:VEVENT", "END;X=1:"+"VEVENT"),
	} {
		// Component framing has no parameters in RFC 5545. Reject it before the
		// recursive decoder can interpret a different stack from our preflight.
		_, _, err := Decode("ics", []byte(raw), []byte(`{"ics_version":1,"ics_event_index":0}`), SourceContext{})
		assert.Error(t, err)
	}
}

func TestICSFoldedComponentLimits(t *testing.T) {
	event := "BEGIN:VE\r\n VENT\r\nUID:folded@example.com\r\nDTSTART:20270302T120000Z\r\nEND:VE\r\n VENT\r\n"
	t.Run("valid", func(t *testing.T) {
		components, err := ParseICS([]byte("BEGIN:VCALENDAR\r\n"+event+"END:VCALENDAR\r\n"), "", SourceContext{})
		require.NoError(t, err)
		require.Len(t, components, 1)
		assert.Equal(t, "folded@example.com", components[0].Event.ICalUID)
	})
	t.Run("event limit", func(t *testing.T) {
		_, err := ParseICS([]byte("BEGIN:VCALENDAR\r\n"+strings.Repeat(event, 1001)+"END:VCALENDAR\r\n"), "", SourceContext{})
		require.ErrorContains(t, err, "1000 VEVENT")
	})
	t.Run("nesting", func(t *testing.T) {
		// Split property names to check the logical-line nesting guard before
		// the dependency's recursive component parser runs.
		nested := "BEGIN:VCALENDAR\r\n" + strings.Repeat("BE\r\n GIN:X\r\n", 33) + strings.Repeat("EN\r\n D:X\r\n", 33) + "END:VCALENDAR\r\n"
		_, err := ParseICS([]byte(nested), "", SourceContext{})
		require.ErrorContains(t, err, "nesting")
	})
}
