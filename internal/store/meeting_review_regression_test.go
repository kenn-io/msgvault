package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingExceptionUIDInverseRecord(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	master := strings.Replace(linkSeriesRaw, `"iCalUID":"weekly@example.org",`, `"status":"cancelled",`, 1)
	_, masterID := calendarFixture(t, st, "master", master)
	_, exception := calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	requirements.NoError(err)
	requirements.Len(record.Links, 1)
	for _, ref := range []OccurrenceRef{{MessageID: exception}, {MessageID: masterID, Key: "20270314T100000Z"}} {
		record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: ref})
		requirements.NoError(err)
		assert.Equal(t, []int64{note}, record.MessageIDs)
	}
}

func TestMeetingSuggestionsExcludeResolvedPairs(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		copies bool
	}{{"automatic", true}, {"link", true}, {"split", true}, {"link", false}, {"split", false}} {
		mode := tc.mode
		t.Run(fmt.Sprintf("%s/copies=%t", mode, tc.copies), func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			uid := "shared@example.org"
			if !tc.copies {
				uid = "suggest@example.org"
			}
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			var copyID int64
			if tc.copies {
				_, copyID = calendarFixture(t, st, "copy", singleLinkRaw("copy", uid), "copy@example.com")
			}
			raw, format := `{"meeting":{"url":"https://meet.example.com/room"}}`, "circleback_json"
			if mode == "automatic" {
				raw, format = `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`, "granola_json"
			}
			data, note := projectionFixture(t, st, "note", format, raw)
			data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
			_, err := st.UpsertMessage(data.Message)
			requirements.NoError(err)
			refs := []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: event}}
			if tc.copies {
				refs = append(refs, MeetingLinkRef{Kind: "evt", MessageID: copyID})
			}
			if !tc.copies {
				for _, ref := range refs {
					page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10)
					requirements.NoError(err)
					requirements.Len(page.Suggestions, 1)
				}
			}
			if mode == "link" {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			if mode == "split" {
				requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			for _, ref := range refs {
				page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10)
				requirements.NoError(err)
				assert.Empty(t, page.Suggestions, "%s %d", ref.Kind, ref.MessageID)
			}
		})
	}
}

func TestMeetingSeriesMasterNeedsOccurrenceKey(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
	err := st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master})
	requirements.Error(err)
	assert.ErrorContains(t, err, "@<occurrence key>")
}
