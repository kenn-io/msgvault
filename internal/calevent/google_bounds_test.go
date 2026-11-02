package calevent

import (
	"fmt"
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
		{date: "2018-11-04", calendarZone: "America/Sao_Paulo", want: "2018-11-04T03:00:00Z"},
		{date: "2027-02-30", bad: true},
		{date: "2027-03-14", calendarZone: "Bad/Zone", bad: true},
		{want: ""},
	} {
		t.Run(tt.date+tt.clock+tt.eventZone+tt.calendarZone, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			event, _, err := Decode("gcal_json", []byte(fmt.Sprintf(`{"id":"bounds","start":{"date":%q,"dateTime":%q,"timeZone":%q}}`, tt.date, tt.clock, tt.eventZone)), nil, tt.calendarZone)
			got := event.Start
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
