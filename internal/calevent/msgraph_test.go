package calevent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/calrecur"
)

func decodeGraph(t *testing.T, raw string) Event {
	t.Helper()
	event, ok, err := Decode(MSGraphRawFormat, []byte(raw), []byte(`{"account_email":"me@example.com"}`), "America/New_York")
	require.NoError(t, err)
	require.True(t, ok)
	return event
}

// expandGraph returns the local start of each occurrence in the first year.
func expandGraph(t *testing.T, e Event) []string {
	t.Helper()
	zone := e.EventTimeZone
	if e.AllDay {
		zone = e.CalendarTimeZone
	}
	loc, err := time.LoadLocation(zone)
	require.NoError(t, err)
	set, err := calrecur.ParseSet(*e.Start, e.AllDay, e.StartDate, loc, e.Recurrence)
	require.NoError(t, err)
	days := 0
	if e.AllDay {
		days = 1
	}
	occ, _, err := calrecur.Expand(context.Background(), set, e.End.Sub(*e.Start), days, *e.Start, e.Start.AddDate(1, 0, 0), 10000)
	require.NoError(t, err)
	var out []string
	for _, o := range occ {
		out = append(out, o.Start.In(loc).Format("2006-01-02 15:04"))
	}
	return out
}

// graphSeries builds a series that starts at 09:00 New York time on day. As in
// Graph, the series start is its first occurrence.
func graphSeries(day, pattern, rng, extra string) string {
	return fmt.Sprintf(`{"id":"series-1","type":"seriesMaster","subject":"Standup",
 "start":{"dateTime":"%[1]sT14:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"%[1]sT14:30:00.0000000","timeZone":"UTC"},
 "originalStartTimeZone":"Eastern Standard Time",
 "recurrence":{"pattern":%[2]s,"range":%[3]s}%[4]s}`, day, pattern, rng, extra)
}

func TestGraphRecurrenceRules(t *testing.T) {
	tests := []struct {
		name, day, pattern, rng string
		rule                    string
		first                   []string
	}{
		{"daily count", "2027-01-05", `{"type":"daily","interval":2}`,
			`{"type":"numbered","numberOfOccurrences":3,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=DAILY;INTERVAL=2;COUNT=3",
			[]string{"2027-01-05 09:00", "2027-01-07 09:00", "2027-01-09 09:00"}},
		{"weekly until inclusive end date", "2027-01-05", `{"type":"weekly","interval":1,"daysOfWeek":["tuesday","thursday"],"firstDayOfWeek":"sunday"}`,
			`{"type":"endDate","endDate":"2027-01-12","recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=WEEKLY;INTERVAL=1;BYDAY=TU,TH;WKST=SU;UNTIL=20270113T045959Z",
			[]string{"2027-01-05 09:00", "2027-01-07 09:00", "2027-01-12 09:00"}},
		{"third tuesday", "2027-01-19", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["tuesday"],"index":"third"}`,
			`{"type":"numbered","numberOfOccurrences":2,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=MONTHLY;INTERVAL=1;BYDAY=3TU;COUNT=2",
			[]string{"2027-01-19 09:00", "2027-02-16 09:00"}},
		{"last weekday", "2027-01-29", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["monday","tuesday","wednesday","thursday","friday"],"index":"last"}`,
			`{"type":"numbered","numberOfOccurrences":2,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=MONTHLY;INTERVAL=1;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1;COUNT=2",
			[]string{"2027-01-29 09:00", "2027-02-26 09:00"}},
		{"absolute monthly", "2027-01-20", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":20}`,
			`{"type":"numbered","numberOfOccurrences":2,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=20;COUNT=2",
			[]string{"2027-01-20 09:00", "2027-02-20 09:00"}},
		{"day 31 falls back to the last day", "2027-01-31", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":31}`,
			`{"type":"numbered","numberOfOccurrences":3,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=28,29,30,31;BYSETPOS=-1;COUNT=3",
			[]string{"2027-01-31 09:00", "2027-02-28 09:00", "2027-03-31 09:00"}},
		{"day 30 falls back in February", "2027-01-30", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":30}`,
			`{"type":"numbered","numberOfOccurrences":3,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=28,29,30;BYSETPOS=-1;COUNT=3",
			[]string{"2027-01-30 09:00", "2027-02-28 09:00", "2027-03-30 09:00"}},
		{"day 29 in a leap year", "2028-01-29", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":29}`,
			`{"type":"numbered","numberOfOccurrences":2,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=28,29;BYSETPOS=-1;COUNT=2",
			[]string{"2028-01-29 09:00", "2028-02-29 09:00"}},
		{"yearly February 29 falls back to the last day", "2028-02-29", `{"type":"absoluteYearly","interval":1,"month":2,"dayOfMonth":29}`,
			`{"type":"numbered","numberOfOccurrences":2,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=YEARLY;INTERVAL=1;BYMONTH=2;BYMONTHDAY=-1;COUNT=2",
			[]string{"2028-02-29 09:00", "2029-02-28 09:00"}},
		{"start off the pattern", "2027-01-01", `{"type":"weekly","interval":1,"daysOfWeek":["tuesday"],"firstDayOfWeek":"sunday"}`,
			`{"type":"numbered","numberOfOccurrences":2,"recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=WEEKLY;INTERVAL=1;BYDAY=TU;WKST=SU;COUNT=2",
			[]string{"2027-01-05 09:00", "2027-01-12 09:00"}},
		{"relative yearly", "2027-02-14", `{"type":"relativeYearly","interval":1,"month":2,"daysOfWeek":["sunday"],"index":"second"}`,
			`{"type":"noEnd","recurrenceTimeZone":"Eastern Standard Time"}`,
			"RRULE:FREQ=YEARLY;INTERVAL=1;BYMONTH=2;BYDAY=2SU",
			[]string{"2027-02-14 09:00"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			e := decodeGraph(t, graphSeries(tt.day, tt.pattern, tt.rng, ""))
			assertions.Equal("series", e.Kind)
			assertions.Equal("America/New_York", e.EventTimeZone)
			assertions.Equal([]string{tt.rule}, e.Recurrence)
			got := expandGraph(t, e)
			requirements.GreaterOrEqual(len(got), len(tt.first))
			assertions.Equal(tt.first, got[:len(tt.first)])
		})
	}
}

func TestGraphCancelledOccurrencesAreExcluded(t *testing.T) {
	assertions := assert.New(t)
	e := decodeGraph(t, graphSeries("2027-01-05", `{"type":"weekly","interval":1,"daysOfWeek":["tuesday"]}`,
		`{"type":"numbered","numberOfOccurrences":3,"recurrenceTimeZone":"Eastern Standard Time"}`,
		`,"cancelledOccurrences":["OID.AAMkSeries=.2027-01-12"]`))
	assertions.Equal([]string{"2027-01-05 09:00", "2027-01-19 09:00"}, expandGraph(t, e))
}

func TestGraphAllDayStartOffThePattern(t *testing.T) {
	assertions := assert.New(t)
	e := decodeGraph(t, `{"id":"bday","type":"seriesMaster","isAllDay":true,
 "start":{"dateTime":"2020-06-01T00:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2020-06-02T00:00:00.0000000","timeZone":"UTC"},
 "recurrence":{"pattern":{"type":"absoluteYearly","interval":1,"month":6,"dayOfMonth":14},
  "range":{"type":"numbered","numberOfOccurrences":2,"recurrenceTimeZone":"Eastern Standard Time"}}}`)
	assertions.Equal("2020-06-14", e.StartDate)
	assertions.Equal("2020-06-15", e.EndDate)
	assertions.Equal([]string{"2020-06-14 00:00"}, expandGraph(t, e), "the second occurrence falls outside the one-year window")
}

func TestGraphAllDayYearly(t *testing.T) {
	assertions := assert.New(t)
	e := decodeGraph(t, `{"id":"bday","type":"seriesMaster","isAllDay":true,
 "start":{"dateTime":"2020-06-14T00:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2020-06-15T00:00:00.0000000","timeZone":"UTC"},
 "recurrence":{"pattern":{"type":"absoluteYearly","interval":1,"month":6,"dayOfMonth":14},
  "range":{"type":"noEnd","endDate":"0001-01-01","recurrenceTimeZone":"Eastern Standard Time"}},
 "cancelledOccurrences":["OID.AAMkBday=.2020-06-14"]}`)
	assertions.True(e.AllDay)
	assertions.Equal("2020-06-14", e.StartDate)
	assertions.Equal("2020-06-15", e.EndDate)
	assertions.Equal([]string{"RRULE:FREQ=YEARLY;INTERVAL=1;BYMONTH=6;BYMONTHDAY=14", "EXDATE;VALUE=DATE:20200614"}, e.Recurrence)
	assertions.Empty(expandGraph(t, e), "the only occurrence in the first year is cancelled")
}

func TestGraphExceptionOriginalStart(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	e := decodeGraph(t, `{"id":"exc-1","type":"exception","seriesMasterId":"series-1",
 "originalStart":"2027-01-12T14:00:00Z","originalStartTimeZone":"Eastern Standard Time",
 "start":{"dateTime":"2027-01-12T16:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-12T16:30:00.0000000","timeZone":"UTC"}}`)
	assertions.Equal("exception", e.Kind)
	assertions.Equal("series-1", e.SeriesProviderID)
	requirements.NotNil(e.Original)
	assertions.Equal(time.Date(2027, 1, 12, 14, 0, 0, 0, time.UTC), e.Original.Instant)
	assertions.Equal(time.Date(2027, 1, 12, 16, 0, 0, 0, time.UTC), *e.Start)
}

func TestGraphUnknownZoneKeepsSeriesUnexpandable(t *testing.T) {
	requirements := require.New(t)
	e := decodeGraph(t, graphSeries("2027-01-05", `{"type":"daily","interval":1}`,
		`{"type":"noEnd","recurrenceTimeZone":"tzone://Microsoft/Custom"}`, ""))
	loc := time.UTC
	_, err := calrecur.ParseSet(*e.Start, false, "", loc, e.Recurrence)
	requirements.ErrorIs(err, calrecur.ErrUnsupportedRecurrence)
}

func TestGraphSeriesWithoutAZoneUsesTheCalendarZone(t *testing.T) {
	e := decodeGraph(t, `{"id":"series-1","type":"seriesMaster","subject":"Standup",
 "start":{"dateTime":"2027-03-09T14:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-03-09T14:30:00.0000000","timeZone":"UTC"},
 "recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["tuesday"]},"range":{"type":"numbered","numberOfOccurrences":2}}}`)
	assert.Equal(t, []string{"2027-03-09 09:00", "2027-03-16 09:00"}, expandGraph(t, e), "the clock stays in New York time across the DST change")
}

func TestGraphPlainOccurrenceIsNotStored(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	_, ok, err := Decode(MSGraphRawFormat, []byte(`{"id":"occ","type":"occurrence"}`), nil, "")
	requirements.NoError(err)
	assertions.False(ok)
}

func TestGraphAttendees(t *testing.T) {
	assertions := assert.New(t)
	e := decodeGraph(t, `{"id":"meeting","type":"singleInstance",
 "start":{"dateTime":"2027-01-05T14:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-05T15:00:00.0000000","timeZone":"UTC"},
 "organizer":{"emailAddress":{"name":"Organizer","address":"Organizer@Example.com"}},
 "attendees":[
  {"type":"required","status":{"response":"accepted"},"emailAddress":{"name":"Organizer","address":"organizer@example.com"}},
  {"type":"required","status":{"response":"tentativelyAccepted"},"emailAddress":{"name":"Me","address":"ME@example.com"}},
  {"type":"optional","status":{"response":"notResponded"},"emailAddress":{"name":"Optional","address":"optional@example.com"}},
  {"type":"resource","status":{"response":"none"},"emailAddress":{"name":"Room 1","address":"room1@example.com"}},
  {"type":"required","status":{"response":"declined"},"emailAddress":{"name":"Declined","address":"declined@example.com"}}
 ]}`)
	assertions.Equal("single", e.Kind)
	assertions.Equal([]Attendee{
		{Email: "organizer@example.com", DisplayName: "Organizer", Role: "organizer", RSVP: "accepted"},
		{Email: "me@example.com", DisplayName: "Me", Role: "required", RSVP: "tentative", IsCopySelf: true},
		{Email: "optional@example.com", DisplayName: "Optional", Role: "optional", RSVP: "needs_action"},
		{Email: "room1@example.com", DisplayName: "Room 1", Role: "room", RSVP: "unknown"},
		{Email: "declined@example.com", DisplayName: "Declined", Role: "required", RSVP: "declined"},
	}, e.Attendees)
}

func TestGraphZone(t *testing.T) {
	assertions := assert.New(t)
	assertions.Equal("America/New_York", GraphZone("Eastern Standard Time"))
	assertions.Equal("America/Phoenix", GraphZone("US Mountain Standard Time"))
	assertions.Equal("Europe/London", GraphZone("Europe/London"))
	assertions.Equal("UTC", GraphZone("tzone://Microsoft/Utc"))
	assertions.Empty(GraphZone(""))
}

func TestGraphMailboxAddressIsSelfForAnAlias(t *testing.T) {
	e, ok, err := Decode(MSGraphRawFormat, []byte(`{"id":"meeting","type":"singleInstance",
 "start":{"dateTime":"2027-01-05T14:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-05T15:00:00.0000000","timeZone":"UTC"},
 "attendees":[{"type":"required","status":{"response":"accepted"},"emailAddress":{"address":"me@example.com"}}]}`),
		[]byte(`{"account_email":"alias@example.com","mailbox_email":"me@example.com"}`), "America/New_York")
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, e.Attendees[0].IsCopySelf)
}

func TestGraphExceptionOriginalKeepsTheSeriesValueType(t *testing.T) {
	tests := []struct {
		name, raw, metadata string
		want                calrecur.Identity
	}{
		{"timed edit of an all-day series",
			`{"id":"exc","type":"exception","seriesMasterId":"s","isAllDay":false,"originalStart":"2027-01-12T05:00:00Z",
 "start":{"dateTime":"2027-01-12T15:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2027-01-12T16:00:00.0000000","timeZone":"UTC"}}`,
			`{"series_all_day":true,"series_time_zone":"America/New_York"}`,
			calrecur.Identity{Date: "2027-01-12", AllDay: true}},
		{"all-day edit of a timed series",
			`{"id":"exc","type":"exception","seriesMasterId":"s","isAllDay":true,"originalStart":"2027-01-12T14:00:00Z",
 "start":{"dateTime":"2027-01-12T00:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2027-01-13T00:00:00.0000000","timeZone":"UTC"}}`,
			`{"series_all_day":false}`,
			calrecur.Identity{Instant: time.Date(2027, 1, 12, 14, 0, 0, 0, time.UTC)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			e, ok, err := Decode(MSGraphRawFormat, []byte(tt.raw), []byte(tt.metadata), "America/New_York")
			requirements.NoError(err)
			requirements.True(ok)
			requirements.NotNil(e.Original)
			assert.Equal(t, tt.want, *e.Original)
		})
	}
}

func TestGraphSupersededEditIsNotProjected(t *testing.T) {
	_, ok, err := Decode(MSGraphRawFormat, []byte(`{"id":"exc","type":"exception","seriesMasterId":"s","originalStart":"2027-01-12T14:00:00Z"}`),
		[]byte(`{"superseded":true}`), "")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGraphAllDayDateReadsConvertedBounds(t *testing.T) {
	assertions := assert.New(t)
	floating := graphDateTime{DateTime: "2027-01-12T00:00:00.0000000", TimeZone: "UTC"}
	assertions.Equal("2027-01-12", GraphAllDayDate(floating, "UTC", "Europe/Berlin"))
	// Berlin midnight converted to UTC falls on the previous UTC date.
	converted := graphDateTime{DateTime: "2027-01-11T23:00:00.0000000", TimeZone: "UTC"}
	assertions.Equal("2027-01-12", GraphAllDayDate(converted, "W. Europe Standard Time", ""))
	assertions.Equal("2027-01-12", GraphAllDayDate(converted, "UTC", "Europe/Berlin"))
	assertions.Equal("2027-01-12", GraphAllDayDate(converted, "tzone://Microsoft/Custom", "Europe/Berlin"))
}
