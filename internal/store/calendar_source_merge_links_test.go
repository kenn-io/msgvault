package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksCalendarSourceMergeUniqueHistoricalEvent(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	history, event := calendarFixture(t, st, "historical-event", singleLinkRaw("historical-event", "history@example.org"), "history")
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
	destination, err := st.GetOrCreateSource("gcal", "destination")
	requirements.NoError(err)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	want := fmt.Sprintf("evt:%d", event)
	requirements.Equal(want, linkedMeetingRefs(t, st, note)[note])

	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination.ID})
	requirements.NoError(err)

	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Equal(want, linkedMeetingRefs(t, st, note)[note])
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(event, page.Occurrences[0].MessageID)
	assertions.Equal(destination.ID, page.Occurrences[0].SourceID)
}
