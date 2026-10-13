package calevent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoogleDateTimeBounds(t *testing.T) {
	for _, tt := range []struct {
		date, clock, eventZone, calendarZone, want string
		bad                                        bool
	}{
		{date: "2027-03-27", calendarZone: "Europe/Paris", want: "2027-03-26T23:00:00Z"},
		{date: "2027-03-29", calendarZone: "Europe/Paris", want: "2027-03-28T22:00:00Z"},
		{date: "2027-03-14", eventZone: "Asia/Tokyo", calendarZone: "America/Los_Angeles", want: "2027-03-13T15:00:00Z"},
		{clock: "2027-03-02T09:00:00-08:00", want: "2027-03-02T17:00:00Z"},
		{clock: "2027-03-02T09:00:00", eventZone: "America/Los_Angeles", want: "2027-03-02T17:00:00Z"},
		{date: "2027-02-30", bad: true},
		{date: "2027-03-14", calendarZone: "Bad/Zone", bad: true},
		{clock: "not a date", bad: true},
		{want: ""},
	} {
		t.Run(tt.date+tt.clock+tt.eventZone+tt.calendarZone, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			got, err := googleInstant(googleDateTime{Date: tt.date, DateTime: tt.clock, TimeZone: tt.eventZone}, tt.calendarZone)
			if tt.bad {
				assertions.Error(err)
				return
			}
			requirements.NoError(err)
			if tt.want == "" {
				assertions.Nil(got)
				return
			}
			requirements.NotNil(got)
			assertions.Equal(tt.want, got.Format(time.RFC3339))
		})
	}
}

func TestGoogleAllDayBoundsPreserveCivilDateWhenMidnightIsSkipped(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	event := decodeGoogle(t, `{"id":"skipped-midnight","start":{"date":"2018-11-04"},"end":{"date":"2018-11-05"}}`, `{}`, SourceContext{CalendarTimeZone: "America/Sao_Paulo"})
	requirements.NotNil(event.Start)
	requirements.NotNil(event.End)
	assertions.Equal("2018-11-04", event.StartDate)
	assertions.Equal("2018-11-05", event.EndDate)
	assertions.Equal("2018-11-04T03:00:00Z", event.Start.Format(time.RFC3339))
	assertions.Equal("2018-11-05T02:00:00Z", event.End.Format(time.RFC3339))
}

func TestGoogleAllDayEndPreservesWhollySkippedCivilDate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	event := decodeGoogle(t, `{"id":"skipped-end","start":{"date":"2011-12-29"},"end":{"date":"2011-12-30"}}`, `{}`, SourceContext{CalendarTimeZone: "Pacific/Apia"})
	requirements.NotNil(event.Start)
	requirements.NotNil(event.End)
	assertions.Equal("2011-12-29", event.StartDate)
	assertions.Equal("2011-12-30", event.EndDate)
	assertions.Equal("2011-12-29T10:00:00Z", event.Start.Format(time.RFC3339))
	assertions.Equal("2011-12-30T10:00:00Z", event.End.Format(time.RFC3339))
}

func TestGoogleWireRejectsInvalidID(t *testing.T) {
	assertions := assert.New(t)
	for _, raw := range []string{`null`, `{}`, `{"id":123}`} {
		_, err := readGoogle([]byte(raw))
		assertions.Error(err)
	}
}
