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

func TestCalendarSyncAfterMergeIntoEmptyRemapsUIDLessLink(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"shared","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	history, incoming := calendarFixture(t, st, "history-event", raw, "history")
	destination, err := st.GetOrCreateSource("gcal", "destination")
	requirements.NoError(err)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: incoming}))

	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination.ID})
	requirements.NoError(err)
	requirements.Equal(fmt.Sprintf("evt:%d", incoming), linkedMeetingRefs(t, st, note)[note])

	// Calendar sync writes the message before its provider JSON.
	conversation, err := st.EnsureConversationWithType(destination.ID, "shared", "calendar", "Example event")
	requirements.NoError(err)
	visible, err := st.UpsertMessage(&Message{ConversationID: conversation, SourceID: destination.ID, SourceMessageID: "shared", MessageType: "calendar_event"})
	requirements.NoError(err)
	requirements.NotEqual(incoming, visible)
	requirements.NoError(st.UpsertMessageRawWithFormat(visible, []byte(raw), "gcal_json"))
	assertions.Equal(fmt.Sprintf("evt:%d", visible), linkedMeetingRefs(t, st, note)[note])
	var staleDecisions int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE target_message_id = ?`, incoming).Scan(&staleDecisions))
	assertions.Zero(staleDecisions)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(visible, page.Occurrences[0].MessageID)
}

func TestMeetingLinksUIDLessCalendarSourceMergeRemapsDecisions(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		incomingDecision     string
		newerVisibleDecision bool
		wantDecision         string
		wantLink             bool
	}{
		{name: "link", incomingDecision: "link", wantDecision: "link", wantLink: true},
		{name: "split", incomingDecision: "split", wantDecision: "split"},
		{name: "newer visible decision wins", incomingDecision: "split", newerVisibleDecision: true, wantDecision: "link", wantLink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"shared","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			history, incoming := calendarFixture(t, st, "history-event", raw, "history")
			destination, visible := calendarFixture(t, st, "destination-event", raw, "destination")
			_, note := projectionFixture(t, st, "note", "twenty_json", `{}`)
			writeDecision := st.LinkMeetingContext
			if tc.incomingDecision == "split" {
				writeDecision = st.UnlinkMeetingContext
			}
			requirements.NoError(writeDecision(t.Context(), note, OccurrenceRef{MessageID: incoming}))
			if tc.newerVisibleDecision {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: visible}))
				later := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
				_, err := st.db.Exec(`UPDATE meeting_link_decisions SET created_at = ? WHERE member_message_id = ? AND target_message_id = ?`, later, note, visible)
				requirements.NoError(err)
			}

			_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination.Message.SourceID})
			requirements.NoError(err)

			var target int64
			var decision string
			requirements.NoError(st.db.QueryRow(`SELECT target_message_id, decision FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&target, &decision))
			assertions.Equal(visible, target)
			assertions.Equal(tc.wantDecision, decision)
			var staleDecisionCount int
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE member_message_id = ? AND target_message_id = ?`, note, incoming).Scan(&staleDecisionCount))
			assertions.Zero(staleDecisionCount)
			linked := linkedMeetingRefs(t, st, note)[note]
			if tc.wantLink {
				assertions.Equal(fmt.Sprintf("evt:%d", visible), linked)
			} else {
				assertions.Empty(linked)
			}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(visible, page.Occurrences[0].MessageID)
		})
	}
}

func TestMeetingLinksUIDLessCalendarSourceMergeRemapsPreviouslyMergedDecisions(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
	}{
		{name: "successive merges into one destination", kind: "successive"},
		{name: "chain destination into another source", kind: "chain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"shared","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`

			var visibleSourceID, visible int64
			if tc.kind == "successive" {
				visibleSource, event := calendarFixture(t, st, "earlier-copy", raw, "earlier-copy")
				visibleSourceID, visible = visibleSource.Message.SourceID, event
			} else {
				visibleSource, event := calendarFixture(t, st, "destination-copy", raw, "destination")
				visibleSourceID, visible = visibleSource.Message.SourceID, event
			}
			history, incoming := calendarFixture(t, st, "history-event", raw, "history")
			middle, err := st.GetOrCreateSource("gcal", "middle")
			requirements.NoError(err)
			_, note := projectionFixture(t, st, "note", "twenty_json", `{}`)
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: incoming}))

			_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: middle.ID})
			requirements.NoError(err)
			assertions.Equal(fmt.Sprintf("evt:%d", incoming), linkedMeetingRefs(t, st, note)[note], "the original event remains visible after the first merge")

			secondFrom, secondInto := visibleSourceID, middle.ID
			if tc.kind == "chain" {
				secondFrom, secondInto = middle.ID, visibleSourceID
			}
			_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: secondFrom, IntoSourceID: secondInto})
			requirements.NoError(err)

			var target int64
			requirements.NoError(st.db.QueryRow(`SELECT target_message_id FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&target))
			assertions.Equal(visible, target)
			assertions.Equal(fmt.Sprintf("evt:%d", visible), linkedMeetingRefs(t, st, note)[note])
			page, err := st.ListCalendarOccurrencesContext(t.Context(), time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(visible, page.Occurrences[0].MessageID)
		})
	}
}
