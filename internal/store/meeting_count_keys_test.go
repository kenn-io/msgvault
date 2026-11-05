package store

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
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
	assertions.ElementsMatch([]string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}, keys[note])
	assertions.NotContains(keys, master)
}
