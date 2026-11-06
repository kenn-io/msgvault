package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingChosenLinkRejectsBroadAutomaticEvidence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		id := fmt.Sprintf("copy-%d", i)
		calendarFixture(t, st, id, singleLinkRaw(id, "crowded@example.org"))
	}
	_, chosen := calendarFixture(t, st, "chosen", singleLinkRaw("chosen", "chosen@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	require.ErrorIs(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: chosen}), ErrQueryTooBroad)
	var decisions int
	require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&decisions))
	assert.Zero(decisions, "a choice the reader cannot return must not be committed")
}

func TestMeetingChosenLinkRejectsBroadTargetCopies(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, chosen := calendarFixture(t, st, "copy-0", singleLinkRaw("copy-0", "crowded@example.org"))
	for i := range 200 {
		id := fmt.Sprintf("copy-%d", i+1)
		calendarFixture(t, st, id, singleLinkRaw(id, "crowded@example.org"))
	}
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z"}}`)
	require.ErrorIs(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: chosen}), ErrQueryTooBroad)
	var decisions int
	require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&decisions))
	assert.Zero(decisions, "a choice the reader cannot return must not be committed")
}

func TestMeetingSuggestOverBudgetKeepsReadableCandidates(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"daily","start":{"dateTime":"1800-01-01T10:00:00Z"},"end":{"dateTime":"1800-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	_, event := calendarFixture(t, st, "single", `{"id":"single","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T10:15:00Z"}}`)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	requirements.NoError(err)
	requirements.Len(page.Suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), page.Suggestions[0].Ref)
	assertions.Equal(note, page.Suggestions[0].MemberMessageID)
	assertions.Equal(30, page.Suggestions[0].OverlapMinutes)
	assertions.Equal([]CalendarUnexpandable{{MessageID: master, Reason: calrecur.ErrBudgetExceeded.Error()}}, page.Unexpandable)
}

func TestMeetingSuggestionsWithOverBudgetEvidence(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		raw := fmt.Sprintf(`{"id":"old-%d","iCalUID":"crowded@example.org","start":{"dateTime":"2020-01-01T10:00:00Z"},"end":{"dateTime":"2020-01-01T11:00:00Z"}}`, i)
		calendarFixture(t, st, fmt.Sprintf("old-%d", i), raw)
	}
	_, event := calendarFixture(t, st, "readable", singleLinkRaw("readable", "readable@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	requirements.NoError(err)
	requirements.Len(page.Suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), page.Suggestions[0].Ref)
}
