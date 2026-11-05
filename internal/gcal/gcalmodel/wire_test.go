package gcalmodel

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeEventDateTime(t *testing.T) {
	for _, tt := range []struct {
		clock, zone, want string
		resolved          bool
	}{
		{"2027-03-02T09:00:00", "America/New_York", "2027-03-02T14:00:00Z", true},
		{"2027-03-16T09:00:00", "America/New_York", "2027-03-16T13:00:00Z", true},
		{"2027-03-02T09:00:00-08:00", "America/New_York", "2027-03-02T17:00:00Z", false},
		{"2027-03-02T09:00:00", "", "", false},
		{"2027-03-02T09:00:00", "Unknown/Zone", "", false},
		{"invalid", "America/New_York", "", false},
	} {
		t.Run(tt.clock+tt.zone, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			bound := fmt.Sprintf(`{"dateTime":%q,"timeZone":%q}`, tt.clock, tt.zone)
			event, err := DecodeEvent([]byte(fmt.Sprintf(`{"id":"example","start":%s,"end":%s,"originalStartTime":%s}`, bound, bound, bound)))
			requirements.NoError(err)
			for _, value := range []EventDateTime{event.Start, event.End, event.OriginalStartTime} {
				if tt.want == "" {
					assertions.True(value.DateTime.IsZero())
				} else {
					assertions.Equal(tt.want, value.DateTime.UTC().Format(time.RFC3339))
				}
				assertions.Equal(tt.resolved, value.ResolvedFromTimeZone)
			}
		})
	}
}
