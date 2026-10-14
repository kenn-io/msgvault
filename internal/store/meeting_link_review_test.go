package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSuggestionsSkipLiveUnsupportedSeriesInBothDirections(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", strings.Replace(linkSeriesRaw, "FREQ=WEEKLY", "FREQ=SECONDLY", 1))
	note := suggestionNoteFixture(t, st)
	for _, ref := range []string{fmt.Sprintf("msg:%d", note), FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270314T100000Z"})} {
		got, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), ref, 10)
		requirements.NoError(err)
		assertions.Empty(got)
	}
}
