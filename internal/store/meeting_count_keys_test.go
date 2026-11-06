package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingCountKeysOneTranscriptTwoOccurrences(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270321T100000Z"}))
	keys, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.ElementsMatch([]MeetingCountKey{
		{Key: "uid:weekly@example.org/20270314T100000Z", Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)},
		{Key: "uid:weekly@example.org/20270321T100000Z", Start: time.Date(2027, 3, 21, 10, 0, 0, 0, time.UTC)},
	}, keys[note])
	assertions.NotContains(keys, master)
}

func TestMeetingCountKeysMovedExceptionKeepsOccurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	raw := `{"id":"moved","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":%q},"end":{"dateTime":%q}}`
	_, exception := calendarFixture(t, st, "moved", fmt.Sprintf(raw, "2027-03-14T12:00:00Z", "2027-03-14T13:00:00Z"))
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	before, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	requirements.Equal([]MeetingCountKey{{Key: "uid:weekly@example.org/20270314T100000Z", Start: time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC)}}, before[note])
	requirements.NoError(st.UpsertMessageRawWithFormat(exception, []byte(fmt.Sprintf(raw, "2027-03-15T14:00:00Z", "2027-03-15T15:00:00Z")), "gcal_json"))
	after, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.Equal([]MeetingCountKey{{Key: "uid:weekly@example.org/20270314T100000Z", Start: time.Date(2027, 3, 15, 14, 0, 0, 0, time.UTC)}}, after[note])
	assertions.NotContains(after, master)
	assertions.NotContains(after, exception)
}
