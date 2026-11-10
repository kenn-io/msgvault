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
	for _, sparse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sparse=%t", sparse), func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			master := strings.Replace(linkSeriesRaw, `"iCalUID":"weekly@example.org",`, `"status":"cancelled",`, 1)
			if sparse {
				master = `{"id":"series","status":"cancelled"}`
			}
			_, masterID := calendarFixture(t, st, "master", master)
			_, exception := calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
			_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
			require.NoError(err)
			require.Len(record.Links, 1)
			assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", masterID), record.Links[0].Ref)
			for _, ref := range []OccurrenceRef{{MessageID: exception}, {MessageID: masterID, Key: "20270314T100000Z"}, {MessageID: copyID, Key: "20270314T100000Z"}} {
				record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: ref})
				require.NoError(err)
				assert.Equal([]int64{note}, record.MessageIDs)
			}
			_, manual := projectionFixture(t, st, "manual", "plaud_json", `{}`)
			require.NoError(st.LinkMeetingContext(t.Context(), manual, OccurrenceRef{MessageID: masterID, Key: "20270314T100000Z"}))
			record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"})
			require.NoError(err)
			assert.Contains(record.MessageIDs, manual)
			calendarFixture(t, st, "cancellation", `{"id":"cancelled","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
			require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: masterID, Key: "20270314T100000Z"}))
			assert.Empty(linkedMeetingRefs(t, st, note)[note])
		})
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

func TestMeetingSuggestionsKeepSplitAfterRecurrenceChange(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	data, note := projectionFixture(t, st, "note", "circleback_json", `{"meeting":{"url":"https://meet.example.com/room"}}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	requirements.NoError(err)
	refs := []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"}}
	for _, ref := range refs {
		page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10)
		requirements.NoError(err)
		requirements.NotEmpty(page.Suggestions)
	}
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1)), "gcal_json"))
	for _, ref := range refs {
		page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10)
		requirements.NoError(err)
		assert.Empty(t, page.Suggestions, "%s", ref.Kind)
	}
}
