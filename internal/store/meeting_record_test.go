package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func repeatedMeetingSeriesFixture(t *testing.T, noteCount int) (*Store, int64, []int64) {
	t.Helper()
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"series","iCalUID":"daily@example.org","start":{"dateTime":"2010-01-01T10:00:00Z"},"end":{"dateTime":"2010-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`
	var event int64
	for i := range 5 {
		_, id := calendarFixture(t, st, fmt.Sprintf("calendar-%d", i), raw, fmt.Sprintf("calendar-%d@example.com", i))
		if i == 0 {
			event = id
		}
	}
	var notes []int64
	for i := range noteCount {
		_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"daily@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
		notes = append(notes, note)
	}
	return st, event, notes
}

func TestMeetingRecordRepeatedSeriesWork(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st, event, notes := repeatedMeetingSeriesFixture(t, 10)
	r := st.meetingReader().withMeetingCache()
	budget := calrecur.RequestReadBudget
	target, err := resolveMeetingOccurrence(t.Context(), r, OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}, &budget)
	require.NoError(err)
	members, err := st.meetingOccurrenceMembers(t.Context(), r, target, &budget)
	require.NoError(err)
	assert.Less(calrecur.RequestReadBudget-budget, 100_000)
	assert.Equal(notes, members)
	for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: event, Key: "20270314T100000Z"}, {Kind: "msg", MessageID: notes[0]}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(err, "record %s:%d", ref.Kind, ref.MessageID)
		assert.Equal(notes, record.MessageIDs)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", event), record.Links[0].Ref)
	}
	_, note := projectionFixture(t, st, "manual", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}))
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	assert.Equal(append(notes, note), record.MessageIDs)
}

func TestMeetingSuggestionsReuseSeriesWork(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st, event, notes := repeatedMeetingSeriesFixture(t, 10)
	ref := fmt.Sprintf("evt:%d@20270314T100000Z", event)
	var candidates []MeetingSuggestion
	for _, note := range notes {
		candidates = append(candidates, MeetingSuggestion{MemberMessageID: note, Ref: ref})
	}
	budget := calrecur.RequestReadBudget
	filtered, err := st.undecidedMeetingSuggestions(t.Context(), st.meetingReader().withMeetingCache(), candidates, 100, &budget)
	assert.Less(calrecur.RequestReadBudget-budget, 100_000)
	require.NoError(err)
	assert.Empty(filtered)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event, Key: "20270314T100000Z"}, 100)
	require.NoError(err)
	assert.Empty(page.Suggestions)
}

func TestMeetingRecordIncludesNotesOnMovedCopy(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	_, exception := calendarFixture(t, st, "moved", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`, "copy@example.com")
	var notes []int64
	for i, start := range []string{"2027-03-14T10:00:00Z", "2027-03-15T12:00:00Z"} {
		_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, start))
		notes = append(notes, note)
		links, err := meetingLinksForTest(t, st, note)
		requirements.NoError(err)
		requirements.Len(links, 1)
	}
	for _, ref := range []OccurrenceRef{{MessageID: master, Key: "20270314T100000Z"}, {MessageID: exception}} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: ref})
		requirements.NoError(err)
		assert.Equal(t, notes, record.MessageIDs)
	}
}

func TestMeetingRecordMovedAllDayCopiesShareOriginalStartsAcrossZones(t *testing.T) {
	for _, start := range []string{"2027-03-14T00:00:00Z", "2027-03-13T15:00:00Z"} {
		t.Run(start, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"series","iCalUID":"all-day@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
			_, utc := calendarFixture(t, st, "utc", raw)
			tokyoData, tokyo := calendarFixture(t, st, "tokyo", raw, "copy@example.com")
			require.NoError(st.UpdateSourceSyncConfig(tokyoData.Message.SourceID, `{"calendar_id":"copy@example.com","time_zone":"Asia/Tokyo"}`))
			_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"all-day@example.org","startsAt":%q}}`, start))
			wantID := utc
			if start == "2027-03-13T15:00:00Z" {
				wantID = tokyo
			}
			wantRef := fmt.Sprintf("evt:%d@20270314", wantID)
			require.Equal(wantRef, linkedMeetingRefs(t, st, note)[note])
			calendarFixture(t, st, "moved", `{"id":"series_20270314","iCalUID":"all-day@example.org","recurringEventId":"series","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`, "copy@example.com")
			require.NoError(st.UpdateSourceSyncConfig(tokyoData.Message.SourceID, `{"calendar_id":"copy@example.com","time_zone":"Asia/Tokyo"}`))
			require.Equal(wantRef, linkedMeetingRefs(t, st, note)[note])
			for _, id := range []int64{tokyo, utc} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id, Key: "20270314"})
				require.NoError(err)
				assert.Equal([]int64{note}, record.MessageIDs, "calendar copy %d", id)
			}
		})
	}
}

func TestMeetingRecordRecurringGoogleEvidence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	projectionFixture(t, st, "other-note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal([]int64{note}, record.MessageIDs)
}

func TestMeetingDecisionsFollowMessageIdentity(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "old@example.org"))
	_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", "old@example.org"), "second@example.com")
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	var count int
	require.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
	assert.Equal(1, count)
	calendarFixture(t, st, "copy", strings.ReplaceAll(singleLinkRaw("copy", "old@example.org"), "old@example.org", "new@example.org"), "second@example.com")
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	assert.Empty(links)
}

func TestMeetingDecisionReplacesUnexpandableCopy(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"unsupported", strings.Replace(linkSeriesRaw, "FREQ=WEEKLY", "FREQ=HOURLY", 1)},
		{"over budget", `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"1800-01-01T10:00:00Z"},"end":{"dateTime":"1800-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
			_, first := calendarFixture(t, st, "first", linkSeriesRaw)
			_, second := calendarFixture(t, st, "second", linkSeriesRaw, "second@example.com")
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`)
			require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: first, Key: "20270314T100000Z"}))
			require.NoError(st.UpsertMessageRawWithFormat(first, []byte(tc.raw), "gcal_json"))
			require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: second, Key: "20270314T100000Z"}))
			require.NoError(st.UpsertMessageRawWithFormat(first, []byte(linkSeriesRaw), "gcal_json"))
			links, err := meetingLinksForTest(t, st, note)
			require.NoError(err)
			require.Len(links, 1)
			assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: automatic}), links[0].Ref)
		})
	}
}

func TestMeetingUnlinkUnavailableOccurrence(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	require.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	calendarFixture(t, st, "master", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1))
	assert.Empty(t, linkedMeetingRefs(t, st, note)[note])
	require.Error(st.LinkMeetingContext(t.Context(), note, ref), "new links still require an available occurrence")
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
	calendarFixture(t, st, "master", linkSeriesRaw)
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	assert.Empty(t, links)
}

func TestMeetingRecordNotesAndCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"automatic notes", func(t *testing.T) {
			t.Helper()
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
			_, a := projectionFixture(t, st, "a", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			_, b := projectionFixture(t, st, "b", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "msg", MessageID: a}, {Kind: "msg", MessageID: b}} {
				record, err := st.MeetingRecordContext(t.Context(), ref)
				require.NoError(t, err)
				assert.Equal(t, []int64{a, b}, record.MessageIDs)
			}
		}},
		{"decisions and copies", func(t *testing.T) {
			t.Helper()
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "record@example.org"))
			_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", "record@example.org"), "second@example.com")
			_, automatic := projectionFixture(t, st, "automatic", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"record@example.org"}}`)
			_, manual := projectionFixture(t, st, "manual", "plaud_json", `{}`)
			_, excluded := projectionFixture(t, st, "excluded", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"record@example.org"}}`)
			require.NoError(st.LinkMeetingContext(t.Context(), manual, OccurrenceRef{MessageID: copyID}))
			require.NoError(st.UnlinkMeetingContext(t.Context(), excluded, OccurrenceRef{MessageID: copyID}))
			for _, id := range []int64{event, copyID} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
				require.NoError(err)
				assert.Equal([]int64{automatic, manual}, record.MessageIDs)
				require.Len(record.Links, 1)
				assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: id}), record.Links[0].Ref)
			}
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: automatic})
			require.NoError(err)
			assert.Equal([]int64{automatic, manual}, record.MessageIDs)
			require.Len(record.Links, 1)
			assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: event}), record.Links[0].Ref)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestMeetingRecordAfterRemovingRecurrence(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "series", linkSeriesRaw)
	_, first := projectionFixture(t, st, "first", "plaud_json", `{}`)
	require.NoError(t, st.LinkMeetingContext(t.Context(), first, OccurrenceRef{MessageID: event, Key: "20270307T100000Z"}))
	calendarFixture(t, st, "series", `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"}}`)
	_, second := projectionFixture(t, st, "second", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-07T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{first, second}, record.MessageIDs)
	for _, note := range []int64{first, second} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		assert.Equal(t, fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
	}
}

func TestMeetingRecordMergedExceptionAlias(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	old := `{"id":"older","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	oldData, _ := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", old, "old@example.com")
	newData, _ := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", `{"id":"replacement","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: oldData.Message.SourceID, IntoSourceID: newData.Message.SourceID})
	require.NoError(t, err)
	_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"old@example.com"},"calendar_event":{"calendar_event_id":"older","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(t, err)
	require.Len(t, record.Links, 1)
	assert.Equal(t, fmt.Sprintf("evt:%d@20270314T100000Z", master), record.Links[0].Ref)
	inverse, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(t, err)
	assert.Equal(t, []int64{note}, inverse.MessageIDs)
}
