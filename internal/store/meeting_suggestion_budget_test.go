package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingSuggestOverBudgetKeepsReadableCandidates(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"daily","start":{"dateTime":"1800-01-01T10:00:00Z"},"end":{"dateTime":"1800-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	_, event := calendarFixture(t, st, "single", `{"id":"single","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T10:15:00Z"}}`)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.NoError(t, err)
	require.Len(t, page.Suggestions, 1)
	assert.Equal(t, fmt.Sprintf("evt:%d", event), page.Suggestions[0].Ref)
	assert.Equal(t, note, page.Suggestions[0].MemberMessageID)
	assert.Equal(t, 30, page.Suggestions[0].OverlapMinutes)
	assert.Equal(t, []CalendarUnexpandable{{MessageID: master, Reason: calrecur.ErrBudgetExceeded.Error()}}, page.Unexpandable)
}
