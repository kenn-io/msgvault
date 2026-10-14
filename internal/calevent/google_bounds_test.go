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
		{date: "2027-03-14", calendarZone: "America/Los_Angeles", want: "2027-03-14T08:00:00Z"},
		{date: "2027-03-15", calendarZone: "America/Los_Angeles", want: "2027-03-15T07:00:00Z"},
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

func TestGoogleWireRejectsInvalidID(t *testing.T) {
	assertions := assert.New(t)
	for _, raw := range []string{`null`, `{}`, `{"id":123}`} {
		_, err := readGoogle([]byte(raw))
		assertions.Error(err)
	}
}
