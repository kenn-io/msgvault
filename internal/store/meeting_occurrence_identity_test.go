package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingDecisionsSurviveStandaloneBecomingRecurring(t *testing.T) {
	for _, tc := range []struct{ name, raw, start, key, decision string }{
		{"timed split", singleLinkRaw("series", "weekly@example.org"), "2027-03-14T10:00:00Z", "20270314T100000Z", "split"},
		{"all-day split", `{"id":"series","iCalUID":"weekly@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`, "2027-03-14T00:00:00Z", "20270314", "split"},
		{"manual link", singleLinkRaw("event", "manual@example.org"), "2027-03-14T10:00:00Z", "20270314T100000Z", "link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", tc.raw)
			var note int64
			if tc.decision == "split" {
				_, note = projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, tc.start))
				require.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
				require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
				require.Empty(linkedMeetingRefs(t, st, note)[note])
			} else {
				_, note = projectionFixture(t, st, "note", "plaud_json", `{}`)
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
				require.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
			}
			calendarFixture(t, st, "event", strings.TrimSuffix(tc.raw, "}")+`,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
			wantRef, wantIDs := "", []int64{}
			if tc.decision == "link" {
				wantRef, wantIDs = fmt.Sprintf("evt:%d@%s", event, tc.key), []int64{note}
			}
			assert.Equal(wantRef, linkedMeetingRefs(t, st, note)[note])
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event, Key: tc.key})
			require.NoError(err)
			assert.Equal(wantIDs, record.MessageIDs)
			if tc.decision == "link" {
				_, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
				assert.ErrorContains(err, "series reference needs @<occurrence key>")
			}
		})
	}
}

func TestMeetingCancelledMasterRetainsGeneratedOccurrenceIdentity(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, first := projectionFixture(t, st, "first", "plaud_json", `{}`)
	_, second := projectionFixture(t, st, "second", "plaud_json", `{}`)
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	for _, note := range []int64{first, second} {
		require.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	}
	want := fmt.Sprintf("evt:%d@%s", master, ref.Key)
	require.Equal(map[int64]string{first: want, second: want}, linkedMeetingRefs(t, st, first, second))
	calendarFixture(t, st, "master", strings.TrimSuffix(linkSeriesRaw, "}")+`,"status":"cancelled"}`)
	assert.Equal(map[int64]string{first: want, second: want}, linkedMeetingRefs(t, st, first, second))
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: ref.Key})
	require.NoError(err)
	assert.Equal([]int64{first, second}, record.MessageIDs)
}

func TestMeetingSparseCancelledMasterPreservesExceptionUID(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, master := calendarFixture(t, st, "master", `{"id":"series","status":"cancelled"}`)
	calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
}

func TestMeetingSparseCancelledExceptionPreservesRecordUID(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", strings.Replace(linkSeriesRaw, `"iCalUID":"weekly@example.org",`, "", 1))
	calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(t, err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingDecisionUsesResolvedOrphanIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, orphanUID, masterUID string
		converged                  bool
	}{
		{"distinct identities", "weekly@example.org", "changed@example.org", false},
		{"converged split", "orphan@example.org", "weekly@example.org", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, orphan := calendarFixture(t, st, "orphan", fmt.Sprintf(`{"id":"series_20270314T100000Z","iCalUID":%q,"recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, tc.orphanUID))
			_, other := calendarFixture(t, st, "other", linkSeriesRaw, "other@example.com")
			_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
			if tc.converged {
				require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: other, Key: "20270314T100000Z"}))
				require.Equal(fmt.Sprintf("evt:%d", orphan), linkedMeetingRefs(t, st, note)[note])
			}
			_, master := calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", tc.masterUID))
			if !tc.converged {
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: other, Key: "20270314T100000Z"}))
			}
			links, err := meetingLinksForTest(t, st, note)
			require.NoError(err)
			if tc.converged {
				assert.Empty(links)
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: other, Key: "20270314T100000Z"})
				require.NoError(err)
				assert.Empty(record.MessageIDs)
			} else {
				require.Len(links, 2)
				assert.ElementsMatch([]string{fmt.Sprintf("evt:%d@20270314T100000Z", master), fmt.Sprintf("evt:%d@20270314T100000Z", other)}, []string{links[0].Ref, links[1].Ref})
			}
		})
	}
}

func TestMeetingGranolaRetainedExceptionAfterRecurrenceRemoval(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	want := fmt.Sprintf("evt:%d@20270314T100000Z", master)
	assert.Equal(want, linkedMeetingRefs(t, st, note)[note])
	require.NoError(st.UpsertMessageRawWithFormat(master, []byte(singleLinkRaw("series", "weekly@example.org")), "gcal_json"))
	assert.Equal(want, linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingRecordIncludesExceptionUIDFromResolvedCopy(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	changed := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", "changed@example.org")
	calendarFixture(t, st, "master", changed)
	_, copyID := calendarFixture(t, st, "copy", changed, "copy@example.com")
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	require.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, note)[note])
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingRecordIncludesFormerCopyGoogleOccurrence(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw, "other@example.com")
	_, copyID := calendarFixture(t, st, "copy", strings.Replace(linkSeriesRaw, `"id":"series"`, `"id":"copy-series"`, 1))
	calendarFixture(t, st, "exception", `{"id":"copy-series_20270314T100000Z","recurringEventId":"copy-series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "copy-series_20270314T100000Z")
	require.NoError(st.UpsertMessageRawWithFormat(copyID, []byte(singleLinkRaw("copy-series", "weekly@example.org")), "gcal_json"))
	require.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", copyID), linkedMeetingRefs(t, st, note)[note])
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingRecordSparseCancelledCopySkipsUnidentifiedNotes(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	cancelled := `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`
	calendarFixture(t, st, "cancelled", cancelled)
	calendarFixture(t, st, "copy-cancelled", cancelled, "copy@example.com")
	logs := captureSlog(t)
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true, SlowMs: 60_000})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })
	ref := MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"}
	_, err := st.MeetingRecordContext(t.Context(), ref)
	require.NoError(err)
	queries := strings.Count(logs.String(), `"msg":"sql"`)
	require.Positive(queries)
	projectionFixture(t, st, "unrelated", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-21T10:00:00Z"}}`)
	logs.Reset()
	record, err := st.MeetingRecordContext(t.Context(), ref)
	require.NoError(err)
	assert.Empty(t, record.MessageIDs)
	assert.Equal(t, queries, strings.Count(logs.String(), `"msg":"sql"`), "unrelated scheduled-only notes must not add candidate reads")
}

func TestMeetingRecordCopyIncludesCancelledMasterDecision(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"series","status":"cancelled"}`)
	calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	require.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, note)[note])
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingRecordIncludesSupersededOrphanDecision(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"orphan","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "replacement", `{"id":"replacement","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
	require.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, note)[note])
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingRecordSparseAllDayCopyUsesCalendarZone(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"series","iCalUID":"weekly@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
	_, master := calendarFixture(t, st, "master", raw)
	copyData, _ := calendarFixture(t, st, "copy", strings.Replace(raw, `"iCalUID":"weekly@example.org",`, "", 1), "copy@example.com")
	calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"date":"2027-03-14"}}`, "copy@example.com")
	require.NoError(st.UpdateSourceSyncConfig(copyData.Message.SourceID, `{"calendar_id":"copy@example.com","time_zone":"Asia/Tokyo"}`))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-13T15:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314"})
	require.NoError(err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingUnlinkCanonicalRefSuppressesAliasLink(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"series","status":"cancelled"}`)
	calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	calendarFixture(t, st, "cancellation", `{"id":"cancelled","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 1)
	require.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	links, err = meetingLinksForTest(t, st, note)
	require.NoError(err)
	assert.Empty(t, links)
}

func TestMeetingGranolaSupersededExceptionKeepsOwnerCalendar(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	raw := `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	calendarFixture(t, st, "exception", raw)
	calendarFixture(t, st, "replacement", strings.Replace(raw, `"id":"edited"`, `"id":"replacement"`, 1))
	note := granolaLinkFixture(t, st, "edited")
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
}

func TestMeetingTwentyLinkSurvivesReschedule(t *testing.T) {
	for _, tc := range []struct{ name, master, exception, start, key, zone, moved string }{
		{"timed", linkSeriesRaw, `{"id":"moved","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`, "2027-03-14T10:00:00Z", "20270314T100000Z", "UTC", "2027-03-15T12:00:00Z"},
		{"all day", `{"id":"series","iCalUID":"weekly@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`, `{"id":"moved","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`, "2027-03-13T15:00:00Z", "20270314", "Asia/Tokyo", "2027-03-14T15:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, master := calendarFixture(t, st, "master", tc.master)
			require.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, fmt.Sprintf(`{"calendar_id":"calendar@example.com","time_zone":%q}`, tc.zone)))
			_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, tc.start))
			want := fmt.Sprintf("evt:%d@%s", master, tc.key)
			require.Equal(want, linkedMeetingRefs(t, st, note)[note])
			calendarFixture(t, st, "exception", tc.exception)
			require.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, fmt.Sprintf(`{"calendar_id":"calendar@example.com","time_zone":%q}`, tc.zone)))
			links, err := meetingLinksForTest(t, st, note)
			require.NoError(err)
			require.Len(links, 1)
			assert.Equal(want, links[0].Ref)
			assert.Equal(tc.moved, links[0].Start.UTC().Format(time.RFC3339))
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: tc.key})
			require.NoError(err)
			assert.Equal([]int64{note}, record.MessageIDs)
			require.Len(record.Links, 1)
			assert.Equal(tc.moved, record.Links[0].Start.UTC().Format(time.RFC3339))
		})
	}
}
