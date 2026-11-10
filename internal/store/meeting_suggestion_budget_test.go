package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingChosenLinkRejectsBroadEvidence(t *testing.T) {
	t.Run("combined budget", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		var targets []OccurrenceRef
		for family := range 2 {
			for copy := range 6 {
				raw := fmt.Sprintf(`{"id":"series-%d","iCalUID":"family-%d@example.org","start":{"dateTime":"1900-01-01T10:00:00Z"},"end":{"dateTime":"1900-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, family, family)
				_, event := calendarFixture(t, st, fmt.Sprintf("family-%d-copy-%d", family, copy), raw, fmt.Sprintf("copy-%d@example.com", copy))
				if copy == 0 {
					targets = append(targets, OccurrenceRef{MessageID: event, Key: "20270314T100000Z"})
				}
			}
		}
		_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
		for _, target := range targets {
			_, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: target.MessageID, Key: target.Key})
			require.NoError(err, "each family is readable on its own")
		}
		require.NoError(st.LinkMeetingContext(t.Context(), note, targets[0]))
		require.ErrorIs(st.LinkMeetingContext(t.Context(), note, targets[1]), ErrQueryTooBroad)
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", targets[0].MessageID), record.Links[0].Ref)
	})
	for _, tc := range []struct {
		name         string
		targetCopies bool
	}{
		{"automatic evidence", false},
		{"target copies", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var chosen int64
			for i := range 201 {
				id := fmt.Sprintf("copy-%d", i)
				_, copyID := calendarFixture(t, st, id, singleLinkRaw(id, "crowded@example.org"))
				if i == 0 {
					chosen = copyID
				}
			}
			raw := `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z"}}`
			if !tc.targetCopies {
				_, chosen = calendarFixture(t, st, "chosen", singleLinkRaw("chosen", "chosen@example.org"))
				raw = `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`
			}
			_, note := projectionFixture(t, st, "note", "twenty_json", raw)
			require.ErrorIs(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: chosen}), ErrQueryTooBroad)
			var decisions int
			require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&decisions))
			assert.Zero(decisions, "a choice the reader cannot return must not be committed")
		})
	}
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

func TestMeetingSuggestionsRejectOverBudgetEvidence(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		raw := fmt.Sprintf(`{"id":"old-%d","iCalUID":"crowded@example.org","start":{"dateTime":"2020-01-01T10:00:00Z"},"end":{"dateTime":"2020-01-01T11:00:00Z"}}`, i)
		calendarFixture(t, st, fmt.Sprintf("old-%d", i), raw)
	}
	calendarFixture(t, st, "readable", singleLinkRaw("readable", "readable@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.ErrorIs(t, err, ErrQueryTooBroad)
}
