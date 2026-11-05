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

func TestMeetingDecisionsSurviveHiddenCalendarCopyAfterMerge(t *testing.T) {
	for _, decision := range []string{"link", "split"} {
		t.Run(decision, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			history, archived := calendarFixture(t, st, "historical", singleLinkRaw("event", "merge@example.org"), "history@example.com")
			_, live := calendarFixture(t, st, "live", singleLinkRaw("event", "merge@example.org"), "calendar@example.com")
			raw := `{"schema_version":1,"recording":{}}`
			if decision == "split" {
				raw = `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"merge@example.org","startsAt":"2027-03-14T10:00:00Z"}}`
			}
			_, note := projectionFixture(t, st, "note", "twenty_json", raw)
			if decision == "link" {
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: archived}))
				require.NotEmpty(linkedMeetingRefs(t, st, note)[note])
			} else {
				require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: archived}))
				require.Empty(linkedMeetingRefs(t, st, note)[note])
			}
			var destination int64
			require.NoError(st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, live).Scan(&destination))
			_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination})
			require.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
			require.NoError(err)
			if decision == "link" {
				assert.NotEmpty(record.Links)
			} else {
				assert.Empty(record.Links)
			}
		})
	}
}

func TestMeetingRecordHiddenCalendarDecisionAfterMerge(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	history, archived := calendarFixture(t, st, "historical", singleLinkRaw("event", "merge@example.org"), "history@example.com")
	liveData, live := calendarFixture(t, st, "live", singleLinkRaw("event", "merge@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: archived}))
	_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: liveData.Message.SourceID})
	requirements.NoError(err)
	for _, event := range []int64{archived, live} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
		requirements.NoError(err)
		assertions.Equal([]int64{note}, record.MessageIDs)
	}
}

func TestMeetingAutomaticLinkAfterHiddenCalendarMerge(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	history, archived := calendarFixture(t, st, "historical", singleLinkRaw("event", "merge@example.org"))
	liveData, live := calendarFixture(t, st, "live", singleLinkRaw("event", "merge@example.org"), "live@example.com")
	note := granolaLinkFixture(t, st, "event")
	requirements.Equal(fmt.Sprintf("evt:%d", archived), linkedMeetingRefs(t, st, note)[note])
	_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: liveData.Message.SourceID})
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("evt:%d", live), linkedMeetingRefs(t, st, note)[note])
	for _, event := range []int64{archived, live} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
		requirements.NoError(err)
		assertions.Equal([]int64{note}, record.MessageIDs)
	}
}
