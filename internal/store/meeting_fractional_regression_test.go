package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksFractionalOrphanRoundTrip(t *testing.T) {
	for _, fraction := range []string{"", ".123456"} {
		t.Run("seconds"+fraction, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := fmt.Sprintf(`{"id":"instance","iCalUID":"fractional@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00%sZ"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, fraction)
			_, orphan := calendarFixture(t, st, "orphan", raw)
			_, note := projectionFixture(t, st, "fractional-note", "plaud_json", `{}`)
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
			links, err := st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			requirements.Len(links, 1)
			assertions.Equal(fmt.Sprintf("evt:%d", orphan), links[0].Ref)
			assertions.Equal(time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC), links[0].Start)
			assertions.Equal(time.Date(2027, 3, 14, 13, 0, 0, 0, time.UTC), links[0].End)
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
			links, err = st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			assertions.Empty(links)
		})
	}
}
