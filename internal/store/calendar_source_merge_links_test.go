package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksCalendarSourceMergeUniqueHistoricalEvent(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%t", automatic), func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			historyMailbox, destinationMailbox, key, uid := "history", "destination", "historical-event", "history@example.org"
			if automatic {
				historyMailbox, destinationMailbox, key, uid = "calendar@example.com", "destination@example.com", "event", "merge@example.org"
			}
			history, event := calendarFixture(t, st, key, singleLinkRaw(key, uid), historyMailbox)
			var note int64
			if automatic {
				note = granolaLinkFixture(t, st, "event")
			} else {
				_, note = projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
			}
			destination, err := st.GetOrCreateSource("gcal", destinationMailbox)
			requirements.NoError(err)
			if !automatic {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			want := fmt.Sprintf("evt:%d", event)
			requirements.Equal(want, linkedMeetingRefs(t, st, note)[note])
			_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination.ID})
			requirements.NoError(err)
			from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 0, 1)})
			requirements.NoError(err)
			assertions.Equal(want, linkedMeetingRefs(t, st, note)[note])
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(event, page.Occurrences[0].MessageID)
			assertions.Equal(destination.ID, page.Occurrences[0].SourceID)
		})
	}
}

func TestMeetingDecisionsSurviveHiddenCalendarCopyAfterMerge(t *testing.T) {
	for _, mode := range []string{"link", "split", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			historyMailbox, liveMailbox := "history@example.com", "calendar@example.com"
			if mode == "automatic" {
				historyMailbox, liveMailbox = "calendar@example.com", "live@example.com"
			}
			history, archived := calendarFixture(t, st, "historical", singleLinkRaw("event", "merge@example.org"), historyMailbox)
			liveData, live := calendarFixture(t, st, "live", singleLinkRaw("event", "merge@example.org"), liveMailbox)
			var note int64
			if mode == "automatic" {
				note = granolaLinkFixture(t, st, "event")
			} else {
				raw := `{"schema_version":1,"recording":{}}`
				if mode == "split" {
					raw = `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"merge@example.org","startsAt":"2027-03-14T10:00:00Z"}}`
				}
				_, note = projectionFixture(t, st, "note", "twenty_json", raw)
				if mode == "link" {
					requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: archived}))
				} else {
					requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: archived}))
				}
			}
			before := linkedMeetingRefs(t, st, note)[note]
			if mode == "split" {
				requirements.Empty(before)
			} else {
				requirements.Equal(fmt.Sprintf("evt:%d", archived), before)
			}
			_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: liveData.Message.SourceID})
			requirements.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
			requirements.NoError(err)
			if mode == "split" {
				assertions.Empty(record.Links)
				return
			}
			requirements.Len(record.Links, 1)
			assertions.Equal(fmt.Sprintf("evt:%d", live), record.Links[0].Ref)
			for _, event := range []int64{archived, live} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
				requirements.NoError(err)
				assertions.Equal([]int64{note}, record.MessageIDs)
			}
			if mode == "link" {
				requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: live}))
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
				requirements.NoError(err)
				assertions.Empty(record.Links)
			}
		})
	}
}
