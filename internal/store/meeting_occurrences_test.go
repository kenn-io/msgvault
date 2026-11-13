package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingOccurrencesMatchRecords(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`)
	_, first := projectionFixture(t, st, "first", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello"}]}]},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled"}`)
	_, second := projectionFixture(t, st, "second", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello"}]}]},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	for _, tc := range []struct {
		note, exception int64
		counted         bool
	}{{first, moved, true}, {second, cancelled, false}} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: tc.note})
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		members := map[int64]bool{}
		for _, row := range rows {
			if row.OccurrenceKey == record.Links[0].Ref {
				members[row.MessageID] = true
				assert.Equal(t, tc.counted, row.Counted)
			}
		}
		assert.Equal(t, map[int64]bool{master: true, tc.exception: true, tc.note: true}, members)
	}
}

func TestMeetingOccurrenceCountsTwoDatesLinkedToOneNote(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, note := projectionFixture(t, st, "note", "meeting_json", `{"transcript":"Notes"}`)
	for _, date := range []string{"20270314T100000Z", "20270321T100000Z"} {
		ref, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@%s", event, date))
		require.NoError(t, err)
		_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, ref, "link")
		require.NoError(t, err)
	}
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 3, 22, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	var eventKeys, noteKeys []string
	for _, row := range rows {
		if row.MessageID == event && row.Counted {
			eventKeys = append(eventKeys, row.OccurrenceKey)
		}
		if row.MessageID == note && row.Counted {
			noteKeys = append(noteKeys, row.OccurrenceKey)
		}
	}
	assert.Len(t, eventKeys, 2)
	assert.Equal(t, eventKeys, noteKeys)
	for _, ref := range []MeetingLinkRef{
		{Kind: "msg", MessageID: note},
		{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270314T100000Z")},
		{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270321T100000Z")},
	} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		assert.Equal(t, []int64{note}, record.MessageIDs)
		var refs []string
		for _, link := range record.Links {
			refs = append(refs, link.Ref)
		}
		assert.ElementsMatch(t, eventKeys, refs)
	}
	_, _ = calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled"}`)
	rows, _, err = st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 3, 22, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	eventKeys, noteKeys = nil, nil
	for _, row := range rows {
		if row.MessageID == event && row.Counted {
			eventKeys = append(eventKeys, row.OccurrenceKey)
		}
		if row.MessageID == note && row.Counted {
			noteKeys = append(noteKeys, row.OccurrenceKey)
		}
	}
	assert.Len(t, eventKeys, 1)
	assert.Equal(t, eventKeys, noteKeys)
}

func TestMeetingOccurrenceCountsCancelledSeriesLeavesAvailableNoteIndependent(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "cancelled-series", `{"id":"series","status":"cancelled","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, note := projectionFixture(t, st, "available-note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270314T100000Z"},"transcript":[{"text":"Notes"}]}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(t, err)
	assert.Empty(t, record.Links)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	counts := map[int64]bool{}
	for _, row := range rows {
		counts[row.MessageID] = row.Counted
	}
	assert.Contains(t, counts, event)
	assert.False(t, counts[event])
	assert.True(t, counts[note])
}

func TestMeetingOccurrenceCountsYearBoundaryAndUnexpandable(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "boundary", `{"id":"boundary","start":{"dateTime":"2026-12-31T23:30:00Z"},"end":{"dateTime":"2027-01-01T00:30:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`)
	_, earlier := calendarFixture(t, st, "earlier", `{"id":"earlier","start":{"dateTime":"2027-02-01T10:00:00Z"},"end":{"dateTime":"2027-02-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1","RDATE:20250601T100000Z"]}`)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	_, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	assert.Empty(t, logs.String())
	_, unsupported := calendarFixture(t, st, "unsupported", `{"id":"unsupported","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
	_, future := calendarFixture(t, st, "future", `{"id":"future","start":{"dateTime":"2028-01-01T10:00:00Z"},"end":{"dateTime":"2028-01-01T11:00:00Z"}}`)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	var warning struct {
		Level          string  `json:"level"`
		Messages       int     `json:"messages"`
		MessageIDs     []int64 `json:"message_ids"`
		TooBroadYears  []int   `json:"too_broad_years"`
		SkippedChoices int     `json:"skipped_choices"`
	}
	require.NoError(t, json.Unmarshal(logs.Bytes(), &warning))
	assert.Equal(t, "WARN", warning.Level)
	assert.Equal(t, 1, warning.Messages)
	assert.Equal(t, []int64{unsupported}, warning.MessageIDs)
	assert.Empty(t, warning.TooBroadYears)
	assert.Zero(t, warning.SkippedChoices)
	var dates, earlierDates []time.Time
	for _, row := range rows {
		if row.MessageID == future {
			assert.False(t, row.Counted)
		}
		if row.MessageID == series {
			assert.True(t, row.Counted)
			dates = append(dates, row.OccurredAt)
		}
		if row.MessageID == earlier && row.Counted {
			earlierDates = append(earlierDates, row.OccurredAt)
		}
	}
	assert.Equal(t, []time.Time{time.Date(2026, 12, 31, 23, 30, 0, 0, time.UTC), time.Date(2027, 1, 1, 23, 30, 0, 0, time.UTC)}, dates)
	assert.Equal(t, []time.Time{time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC), time.Date(2027, 2, 1, 10, 0, 0, 0, time.UTC)}, earlierDates)
	duration := float64(3600)
	assert.Contains(t, rows, MeetingOccurrence{MessageID: unsupported, AnchorMessageID: unsupported, OccurrenceKey: fmt.Sprintf("fallback:%d", unsupported), OccurredAt: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Counted: true, ScheduledSeconds: &duration})
	assert.Contains(t, rows, MeetingOccurrence{MessageID: future, AnchorMessageID: future, OccurrenceKey: fmt.Sprintf("evt:%d", future), OccurredAt: time.Date(2028, 1, 1, 10, 0, 0, 0, time.UTC), DeclinedEmails: []string{}, ScheduledSeconds: &duration})
	rows, _, err = st.MeetingOccurrencesContext(t.Context(), time.Date(2028, 1, 2, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	assert.Contains(t, rows, MeetingOccurrence{MessageID: future, AnchorMessageID: future, OccurrenceKey: fmt.Sprintf("evt:%d", future), OccurredAt: time.Date(2028, 1, 1, 10, 0, 0, 0, time.UTC), Counted: true, DeclinedEmails: []string{}, ScheduledSeconds: &duration})
}

func TestMeetingOccurrenceCountsOwnerDeclineAndNoteAvailability(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "declined", `{"id":"declined","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"calendar@example.com","responseStatus":"declined"}]}`, "delegate@example.com")
	require.NoError(t, st.AddAccountIdentity(data.Message.SourceID, "calendar@example.com", "manual"))
	noteData, note := projectionFixture(t, st, "note", "meeting_json", `{}`)
	_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
	require.NoError(t, err)
	read := func() map[int64]bool {
		t.Helper()
		rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
		require.NoError(t, err)
		counts := map[int64]bool{}
		for _, row := range rows {
			counts[row.MessageID] = row.Counted
		}
		return counts
	}
	assert.Equal(t, map[int64]bool{event: false, note: false}, read())
	noteData.RawMIME = []byte(`{"transcript":"Notes"}`)
	_, err = st.PersistMessage(noteData)
	require.NoError(t, err)
	assert.Equal(t, map[int64]bool{event: true, note: true}, read())
}

func TestMeetingOccurrencesMatchRecordsAmbiguousUID(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, single := calendarFixture(t, st, "single", `{"id":"single","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, series := calendarFixture(t, st, "series", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T09:00:00Z"},"end":{"dateTime":"2027-03-14T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, _ = calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T09:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, first := projectionFixture(t, st, "first", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello"}]}]},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "second", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello"}]}]},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: first})
	require.NoError(t, err)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	var noteKeys, eventKeys []string
	var noteIDs []int64
	for _, row := range rows {
		if row.MessageID == first || row.MessageID == second {
			assert.True(t, row.Counted)
			noteKeys = append(noteKeys, row.OccurrenceKey)
			noteIDs = append(noteIDs, row.MessageID)
		}
		if row.MessageID == single || row.MessageID == series {
			eventKeys = append(eventKeys, row.OccurrenceKey)
		}
	}
	assert.Empty(t, record.Links)
	assert.ElementsMatch(t, record.MessageIDs, noteIDs)
	require.Len(t, noteKeys, 2)
	assert.Equal(t, noteKeys[0], noteKeys[1])
	assert.NotContains(t, eventKeys, noteKeys[0])
}

func TestMeetingOccurrenceCountsAllDayCivilDate(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "all-day", `{"id":"all-day","iCalUID":"allday@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"Asia/Tokyo"}`))
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
	require.NoError(t, err)
	require.Len(t, record.Links, 1)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, record.Links[0].Ref, rows[0].OccurrenceKey)
	assert.Equal(t, time.Date(2027, 3, 13, 15, 0, 0, 0, time.UTC), rows[0].OccurredAt)
	assert.Equal(t, "2027-03-14", rows[0].StartDate)
	assert.True(t, rows[0].Counted)
}

func TestMeetingOccurrenceCountsSavedCalendarLinkAndSplit(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "first-copy", `{"id":"first","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, second := calendarFixture(t, st, "second-copy", `{"id":"second","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "other@example.com")
	left, right := MeetingLinkRef{Kind: "evt", MessageID: first}, MeetingLinkRef{Kind: "evt", MessageID: second}
	_, err := st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	require.NoError(t, err)
	readKeys := func() []string {
		t.Helper()
		rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
		require.NoError(t, err)
		var keys []string
		for _, row := range rows {
			if row.Counted {
				keys = append(keys, row.OccurrenceKey)
			}
		}
		return keys
	}
	keys := readKeys()
	require.Len(t, keys, 2)
	assert.Equal(t, keys[0], keys[1])
	for _, ref := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		require.Len(t, record.Links, 2)
		assert.Contains(t, []string{record.Links[0].Ref, record.Links[1].Ref}, keys[0])
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
	require.NoError(t, err)
	keys = readKeys()
	require.Len(t, keys, 2)
	assert.NotEqual(t, keys[0], keys[1])
	for _, ref := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		assert.Contains(t, keys, record.Links[0].Ref)
	}
}

func TestMeetingOccurrencesMatchRecordsSavedSplit(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "first", `{"id":"first","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, second := calendarFixture(t, st, "second", `{"id":"second","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "other@example.com")
	left, right := MeetingLinkRef{Kind: "evt", MessageID: first}, MeetingLinkRef{Kind: "evt", MessageID: second}
	record, err := st.MeetingRecordContext(t.Context(), left)
	require.NoError(t, err)
	require.Len(t, record.Links, 2)
	_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
	require.NoError(t, err)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	duration := float64(3600)
	for _, ref := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		assert.Contains(t, rows, MeetingOccurrence{MessageID: ref.MessageID, AnchorMessageID: ref.MessageID, OccurrenceKey: record.Links[0].Ref, OccurredAt: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Counted: true, DeclinedEmails: []string{}, ScheduledSeconds: &duration})
	}
}

func TestMeetingOccurrenceCountsReportsBusyYear(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "cap-master", calendarRaw)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", 2001)
	_, err := st.db.Exec(`UPDATE calendar_events SET start_at = '2027-03-01 00:00:00+00:00', end_at = '2027-03-02 00:00:00+00:00', start_date = '2027-03-01', end_date = '2027-03-02', recurrence_json = '["RRULE:FREQ=DAILY"]' WHERE kind = 'series'`)
	require.NoError(t, err)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	_, _, err = st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	var warning struct {
		Level          string  `json:"level"`
		Messages       int     `json:"messages"`
		MessageIDs     []int64 `json:"message_ids"`
		TooBroadYears  []int   `json:"too_broad_years"`
		SkippedChoices int     `json:"skipped_choices"`
	}
	require.NoError(t, json.Unmarshal(logs.Bytes(), &warning))
	assert.Equal(t, "WARN", warning.Level)
	assert.Equal(t, 2002, warning.Messages)
	assert.Equal(t, []int64{event, 100000, 100001, 100002, 100003, 100004, 100005, 100006, 100007, 100008}, warning.MessageIDs)
	assert.Equal(t, []int{2027}, warning.TooBroadYears)
	assert.Zero(t, warning.SkippedChoices)
}

func TestMeetingOccurrenceCountsDeletedFromSource(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "deleted", `{"id":"deleted","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
	_, err := st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`, event)
	require.NoError(t, err)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Counted)
	assert.Equal(t, time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC), rows[0].OccurredAt)
}

func TestMeetingOccurrencesShareDateAnchorAndDeclines(t *testing.T) {
	for _, tc := range []struct {
		name, earlyResponse, lateResponse string
	}{
		{"decline on early copy", "declined", "accepted"},
		{"decline on late copy", "accepted", "declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			_, early := calendarFixture(t, st, "early", fmt.Sprintf(`{"id":"early","start":{"dateTime":"2024-12-29T10:00:00Z"},"end":{"dateTime":"2024-12-29T11:00:00Z"},"attendees":[{"email":"person@example.org","responseStatus":%q}]}`, tc.earlyResponse))
			_, late := calendarFixture(t, st, "late", fmt.Sprintf(`{"id":"late","start":{"dateTime":"2025-01-03T10:00:00Z"},"end":{"dateTime":"2025-01-03T11:00:00Z"},"attendees":[{"email":"person@example.org","responseStatus":%q}]}`, tc.lateResponse))
			_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: early}, MeetingLinkRef{Kind: "evt", MessageID: late}, "link")
			require.NoError(t, err)
			rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC), nil)
			require.NoError(t, err)
			require.Len(t, rows, 2)
			for _, row := range rows {
				assert.Equal(t, time.Date(2024, 12, 29, 10, 0, 0, 0, time.UTC), row.OccurredAt)
				assert.Equal(t, early, row.AnchorMessageID)
				assert.True(t, row.Counted)
				assert.Equal(t, []string{"person@example.org"}, row.DeclinedEmails)
			}
		})
	}
}

func TestMeetingOccurrencesDeletedCalendarUsesAvailableTranscript(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "deleted-event", `{"id":"deleted-event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "available-note", "meeting_json", `{"transcript":"Notes"}`)
	_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
	require.NoError(t, err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`, event)
	require.NoError(t, err)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, note, row.AnchorMessageID)
		assert.Equal(t, row.MessageID == note, row.Counted)
	}
}

func TestMeetingOccurrencesExcludePendingAndHiddenCopies(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "live-event", `{"id":"live-event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
	_, hidden := calendarFixture(t, st, "hidden-event", `{"id":"hidden-event","start":{"dateTime":"2024-01-03T10:00:00Z"},"end":{"dateTime":"2024-01-03T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "pending-note", "meeting_json", `{}`)
	_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
	require.NoError(t, err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, hidden)
	require.NoError(t, err)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	require.NoError(t, err)
	counted := map[int64]bool{}
	for _, row := range rows {
		counted[row.MessageID] = row.Counted
	}
	assert.Equal(t, map[int64]bool{event: true, hidden: false, note: false}, counted)
}
