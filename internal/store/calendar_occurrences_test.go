package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func occurrenceTime(t *testing.T, value string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return v
}

func occurrenceSource(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gcal", name)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"primary","time_zone":"UTC"}`))
	return src.ID
}

func putOccurrenceEvent(t *testing.T, st *store.Store, src int64, event map[string]any) int64 {
	t.Helper()
	providerID, ok := event["id"].(string)
	require.True(t, ok, "event fixture requires a string id")
	conv, err := st.EnsureConversation(src, providerID, "Calendar fixture")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src, ConversationID: conv,
		SourceMessageID: providerID, MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true}})
	require.NoError(t, err)
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRawWithFormat(id, raw, "gcal_json"))
	return id
}

func putOccurrenceEventWithID(t *testing.T, st *store.Store, src, messageID int64, event map[string]any) int64 {
	t.Helper()
	providerID, ok := event["id"].(string)
	require.True(t, ok, "event fixture requires a string id")
	conv, err := st.EnsureConversation(src, providerID, "Calendar fixture")
	require.NoError(t, err)
	insert := `
		INSERT INTO messages (id,conversation_id,source_id,source_message_id,message_type,subject)
		VALUES (?,?,?,?, 'calendar_event', ?) `
	if st.IsPostgreSQL() {
		insert = `
			INSERT INTO messages (id,conversation_id,source_id,source_message_id,message_type,subject)
			OVERRIDING SYSTEM VALUE
			VALUES (?,?,?,?, 'calendar_event', ?)`
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(insert), messageID, conv, src, providerID, "Planning")
	require.NoError(t, err)
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRawWithFormat(messageID, raw, "gcal_json"))
	return messageID
}

func timedOccurrenceEvent(id, start string) map[string]any {
	dt, _ := time.Parse(time.RFC3339, start)
	return map[string]any{"id": id, "summary": "Planning", "status": "confirmed", "iCalUID": "planning@example.com",
		"start": map[string]any{"dateTime": start}, "end": map[string]any{"dateTime": dt.Add(time.Hour).Format(time.RFC3339)},
		"attendees": []map[string]any{{"email": "invited@example.com", "responseStatus": "accepted"}}}
}

func occurrenceWindow(t *testing.T) store.CalendarOccurrenceFilter {
	t.Helper()
	return store.CalendarOccurrenceFilter{From: occurrenceTime(t, "2027-03-01T00:00:00Z"), To: occurrenceTime(t, "2027-04-12T00:00:00Z")}
}

func TestOccurrencesAcceptance1(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("weekly", "2027-03-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=WEEKLY;COUNT=6", "EXDATE:20270315T100000Z"}
	id := putOccurrenceEvent(t, st, src, master)
	cancelled := map[string]any{"id": "weekly_20270308T100000Z", "recurringEventId": "weekly", "status": "cancelled", "originalStartTime": map[string]any{"dateTime": "2027-03-08T10:00:00Z"}}
	putOccurrenceEvent(t, st, src, cancelled)
	moved := timedOccurrenceEvent("weekly_20270322T100000Z", "2027-03-23T12:00:00Z")
	moved["recurringEventId"] = "weekly"
	moved["originalStartTime"] = map[string]any{"dateTime": "2027-03-22T10:00:00Z"}
	putOccurrenceEvent(t, st, src, moved)
	f := occurrenceWindow(t)
	f.IncludeCancelled = true
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 5)
	var golden []map[string]any
	for _, o := range page.Items {
		golden = append(golden, map[string]any{"ref": o.Ref, "start": o.Start.UTC().Format(time.RFC3339), "cancelled": o.Cancelled, "bounds_known": o.BoundsKnown})
	}
	got, err := json.Marshal(golden)
	requirements.NoError(err)
	assertions.JSONEq(fmt.Sprintf(`[{"ref":"evt:%d@20270301T100000Z","start":"2027-03-01T10:00:00Z","cancelled":false,"bounds_known":true},{"ref":"evt:%d@20270308T100000Z","start":"2027-03-08T10:00:00Z","cancelled":true,"bounds_known":true},{"ref":"evt:%d@20270322T100000Z","start":"2027-03-23T12:00:00Z","cancelled":false,"bounds_known":true},{"ref":"evt:%d@20270329T100000Z","start":"2027-03-29T10:00:00Z","cancelled":false,"bounds_known":true},{"ref":"evt:%d@20270405T100000Z","start":"2027-04-05T10:00:00Z","cancelled":false,"bounds_known":true}]`, id, id, id, id, id), string(got))
	f.IncludeCancelled = false
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	assertions.Len(page.Items, 4)
}

func TestOccurrencesZeroIDCancelledExceptionDoesNotCancelSeries(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("zero-exception", "2027-03-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=DAILY;COUNT=3"}
	masterID := putOccurrenceEvent(t, st, src, master)
	exception := map[string]any{
		"id":                "zero-exception_20270302T100000Z",
		"recurringEventId":  "zero-exception",
		"status":            "cancelled",
		"originalStartTime": map[string]any{"dateTime": "2027-03-02T10:00:00Z"},
	}
	putOccurrenceEventWithID(t, st, src, 0, exception)

	f := occurrenceWindow(t)
	f.IncludeCancelled = true
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 3)
	assertions.False(page.Items[0].Cancelled)
	assertions.Equal(masterID, page.Items[0].SeriesMessageID)
	assertions.True(page.Items[1].Cancelled)
	assertions.Equal(int64(0), page.Items[1].MessageID)
	assertions.False(page.Items[2].Cancelled)

	f.IncludeCancelled = false
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 2)
	assertions.Equal(fmt.Sprintf("evt:%d@20270301T100000Z", masterID), page.Items[0].Ref)
	assertions.Equal(fmt.Sprintf("evt:%d@20270303T100000Z", masterID), page.Items[1].Ref)
}

func TestOccurrencesWindowTooLarge(t *testing.T) {
	st := testutil.NewTestStore(t)
	f := occurrenceWindow(t)
	f.To = f.From.Add(401 * 24 * time.Hour)
	_, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	assert.ErrorIs(t, err, store.ErrInvalidArgument)
}

func TestGetOccurrenceOutOfScopeNotFound(t *testing.T) {
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	id := putOccurrenceEvent(t, st, src, timedOccurrenceEvent("one", "2027-03-01T10:00:00Z"))
	_, err := st.GetCalendarOccurrenceContext(context.Background(), store.SourceScope{}, fmt.Sprintf("evt:%d", id))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestOccurrencesMovedExceptionOfFinishedMaster(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("finished", "2026-01-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=WEEKLY;COUNT=2"}
	id := putOccurrenceEvent(t, st, src, master)
	moved := timedOccurrenceEvent("finished_20260108T100000Z", "2027-03-03T10:00:00Z")
	moved["recurringEventId"] = "finished"
	moved["originalStartTime"] = map[string]any{"dateTime": "2026-01-08T10:00:00Z"}
	exceptionID := putOccurrenceEvent(t, st, src, moved)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20260108T100000Z", id), page.Items[0].Ref)
	assertions.Equal(exceptionID, page.Items[0].MessageID)
	got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), page.Items[0].Ref)
	requirements.NoError(err)
	assertions.Equal(page.Items[0], got)
}

func TestOccurrencesAttendeeFilterUsesEffectiveAttendees(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("weekly", "2027-03-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=WEEKLY;COUNT=2"}
	putOccurrenceEvent(t, st, src, master)
	exc := timedOccurrenceEvent("weekly_20270308T100000Z", "2027-03-08T10:00:00Z")
	exc["recurringEventId"] = "weekly"
	exc["originalStartTime"] = map[string]any{"dateTime": "2027-03-08T10:00:00Z"}
	exc["attendees"] = []map[string]any{{"email": "replacement@example.com"}}
	id := putOccurrenceEvent(t, st, src, exc)
	f := occurrenceWindow(t)
	f.AttendeeEmail = " REPLACEMENT@example.com "
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal(id, page.Items[0].MessageID)
	f.AttendeeEmail = "invited@example.com"
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.NotEqual(id, page.Items[0].MessageID)
}

func TestOccurrencesBudgetMarksUnexpandable(t *testing.T) {
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("old", "1800-01-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=DAILY"}
	id := putOccurrenceEvent(t, st, src, master)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	assert.Equal(t, []store.UnexpandableSeries{{Ref: fmt.Sprintf("evt:%d", id), Reason: "budget"}}, page.Unexpandable)
}

func TestOccurrencesCursorStable(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	for i := range 5 {
		putOccurrenceEvent(t, st, src, timedOccurrenceEvent(fmt.Sprintf("single%d", i), fmt.Sprintf("2027-03-0%dT10:00:00Z", i+1)))
	}
	f := occurrenceWindow(t)
	f.Limit = 2
	first, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(first.Items, 2)
	requirements.NotEmpty(first.NextCursor)
	f.Cursor = first.NextCursor
	second, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(second.Items, 2)
	again, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	assertions.Equal(second, again)
	assertions.True(first.Items[1].Start.Before(second.Items[0].Start))
	f.Cursor = second.NextCursor
	last, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	assertions.Len(last.Items, 1)
	assertions.Empty(last.NextCursor)
	f.AttendeeEmail = "other@example.com"
	_, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	assertions.ErrorIs(err, store.ErrInvalidArgument)
}

func TestOccurrencesReversedIDAndTimeOrder(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	late := putOccurrenceEvent(t, st, src, timedOccurrenceEvent("late", "2027-03-01T15:00:00Z"))
	early := putOccurrenceEvent(t, st, src, timedOccurrenceEvent("early", "2027-03-01T09:00:00Z"))
	f := occurrenceWindow(t)
	f.Limit = 1
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal(early, page.Items[0].MessageID)
	f.Cursor = page.NextCursor
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal(late, page.Items[0].MessageID)
}

func TestOccurrencesTwoCalendarsSameUIDListedPerSource(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	a := occurrenceSource(t, st, "a@example.com")
	b := occurrenceSource(t, st, "b@example.com")
	for _, src := range []int64{a, b} {
		putOccurrenceEvent(t, st, src, timedOccurrenceEvent("copy", "2027-03-01T10:00:00Z"))
	}
	f := occurrenceWindow(t)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	assertions.Len(page.Items, 2)
	f.SourceIDs = []int64{b}
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.SourceScope{IDs: []int64{a}}, f)
	requirements.NoError(err)
	assertions.Empty(page.Items)
}

func TestOccurrencesExcludeLocallyDeleted(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	id := putOccurrenceEvent(t, st, src, timedOccurrenceEvent("gone", "2027-03-01T10:00:00Z"))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?"), id)
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	requirements.NoError(err)
	assertions.Empty(page.Items)
	_, err = st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), fmt.Sprintf("evt:%d", id))
	assertions.ErrorIs(err, store.ErrNotFound)
}

func TestOccurrenceTombstoneUnknownBounds(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	id := putOccurrenceEvent(t, st, src, map[string]any{"id": "orphan_20270301T100000Z", "recurringEventId": "orphan", "status": "cancelled", "originalStartTime": map[string]any{"dateTime": "2027-03-01T10:00:00Z"}})
	got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), fmt.Sprintf("evt:%d", id))
	requirements.NoError(err)
	assertions.False(got.BoundsKnown)
	assertions.True(got.Cancelled)
	assertions.Equal(got.Start, got.End)
	assertions.Equal(occurrenceTime(t, "2027-03-01T10:00:00Z"), got.Start)
	f := occurrenceWindow(t)
	f.IncludeCancelled = true
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal(got, page.Items[0])
	master := timedOccurrenceEvent("orphan", "2027-03-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=WEEKLY;COUNT=2"}
	masterID := putOccurrenceEvent(t, st, src, master)
	got, err = st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), fmt.Sprintf("evt:%d", id))
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("evt:%d@20270301T100000Z", masterID), got.Ref)
	assertions.True(got.BoundsKnown)
}

func TestOccurrencesTooBroadErrors(t *testing.T) {
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	for i := range 2100 {
		e := timedOccurrenceEvent(fmt.Sprintf("series%d", i), "2027-03-01T10:00:00Z")
		e["recurrence"] = []string{"RRULE:FREQ=WEEKLY"}
		putOccurrenceEvent(t, st, src, e)
	}
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	require.ErrorIs(t, err, store.ErrQueryTooBroad)
	assert.Empty(t, page.Items)
	assert.Empty(t, page.NextCursor)
}

func TestOccurrencesRequestBudgetErrors(t *testing.T) {
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	for i := range 12 {
		e := timedOccurrenceEvent(fmt.Sprintf("old%d", i), "1800-01-01T10:00:00Z")
		e["recurrence"] = []string{"RRULE:FREQ=DAILY"}
		putOccurrenceEvent(t, st, src, e)
	}
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	require.ErrorIs(t, err, store.ErrQueryTooBroad)
	assert.Empty(t, page.Items)
	assert.Empty(t, page.NextCursor)
}

func TestOccurrencesRDateOnlySeriesExhaustRequestBudget(t *testing.T) {
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	start := occurrenceTime(t, "2027-03-01T10:00:00Z")
	rdates := make([]string, calrecur.SeriesReadBudget+1)
	for i := range rdates {
		rdates[i] = start.Add(time.Duration(i+1) * time.Hour).UTC().Format("20060102T150405Z")
	}
	recurrence := "RDATE:" + strings.Join(rdates, ",")
	for i := range 11 {
		event := timedOccurrenceEvent(fmt.Sprintf("rdate-only-%d", i), start.Format(time.RFC3339))
		event["recurrence"] = []string{recurrence}
		putOccurrenceEvent(t, st, src, event)
	}
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	require.ErrorIs(t, err, store.ErrQueryTooBroad)
	assert.Empty(t, page.Items)
	assert.Empty(t, page.NextCursor)
}

func TestOccurrencesAllDayCurrentZoneAndGet(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	requirements.NoError(st.UpdateSourceSyncConfig(src, `{"calendar_id":"primary","time_zone":"Pacific/Kiritimati"}`))
	e := map[string]any{"id": "all-day", "status": "confirmed", "start": map[string]any{"date": "2027-03-14"}, "end": map[string]any{"date": "2027-03-15"}, "recurrence": []string{"RRULE:FREQ=WEEKLY;COUNT=2"}}
	id := putOccurrenceEvent(t, st, src, e)
	requirements.NoError(st.UpdateSourceSyncConfig(src, `{"calendar_id":"primary","time_zone":"Pacific/Honolulu"}`))
	f := store.CalendarOccurrenceFilter{From: occurrenceTime(t, "2027-03-15T09:00:00Z"), To: occurrenceTime(t, "2027-03-15T10:00:00Z")}
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	got := page.Items[0]
	assertions.Equal(fmt.Sprintf("evt:%d@20270314", id), got.Ref)
	assertions.Equal("2027-03-14", got.StartDate)
	assertions.Equal("2027-03-15", got.EndDate)
	assertions.Equal(occurrenceTime(t, "2027-03-14T10:00:00Z"), got.Start.UTC())
	detail, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), got.Ref)
	requirements.NoError(err)
	assertions.Equal(got, detail)
	f.From, f.To = occurrenceTime(t, "2027-03-21T10:00:00Z"), occurrenceTime(t, "2027-03-22T10:00:00Z")
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal("2027-03-21", page.Items[0].StartDate)
	assertions.Equal("2027-03-22", page.Items[0].EndDate)
}

func TestStandaloneAllDayOccurrenceSkippedMidnightListGet(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	requirements.NoError(st.UpdateSourceSyncConfig(src, `{"calendar_id":"primary","time_zone":"America/Sao_Paulo"}`))
	id := putOccurrenceEvent(t, st, src, map[string]any{
		"id": "skipped-midnight-single", "summary": "Planning", "status": "confirmed",
		"start": map[string]any{"date": "2018-11-04"}, "end": map[string]any{"date": "2018-11-05"},
	})
	f := store.CalendarOccurrenceFilter{
		From: occurrenceTime(t, "2018-11-04T03:00:00Z"),
		To:   occurrenceTime(t, "2018-11-04T04:00:00Z"),
	}
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	listed := page.Items[0]
	assertions.Equal(fmt.Sprintf("evt:%d", id), listed.Ref)
	assertions.Equal(occurrenceTime(t, "2018-11-04T03:00:00Z"), listed.Start.UTC())
	assertions.Equal(occurrenceTime(t, "2018-11-05T02:00:00Z"), listed.End.UTC())
	detail, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), listed.Ref)
	requirements.NoError(err)
	assertions.Equal(listed, detail)
}

func TestAllDayOccurrenceDetailSkippedMidnight(t *testing.T) {
	for _, tc := range []struct {
		name      string
		series    map[string]any
		exception map[string]any
		key       string
	}{
		{
			name: "nominal occurrence",
			series: map[string]any{
				"id": "skipped-midnight-series", "summary": "Planning", "status": "confirmed",
				"start": map[string]any{"date": "2018-11-04"}, "end": map[string]any{"date": "2018-11-05"},
				"recurrence": []string{"RRULE:FREQ=DAILY;COUNT=1"},
			},
			key: "20181104",
		},
		{
			name: "moved exception",
			series: map[string]any{
				"id": "skipped-midnight-exception", "summary": "Planning", "status": "confirmed",
				"start": map[string]any{"date": "2018-11-03"}, "end": map[string]any{"date": "2018-11-04"},
				"recurrence": []string{"RRULE:FREQ=DAILY;COUNT=2"},
			},
			exception: map[string]any{
				"id": "skipped-midnight-exception_20181103", "recurringEventId": "skipped-midnight-exception",
				"summary": "Planning", "status": "confirmed",
				"originalStartTime": map[string]any{"date": "2018-11-03"},
				"start":             map[string]any{"date": "2018-11-04"}, "end": map[string]any{"date": "2018-11-05"},
			},
			key: "20181103",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			src := occurrenceSource(t, st, "owner@example.com")
			requirements.NoError(st.UpdateSourceSyncConfig(src, `{"calendar_id":"primary","time_zone":"America/Sao_Paulo"}`))
			id := putOccurrenceEvent(t, st, src, tc.series)
			if tc.exception != nil {
				putOccurrenceEvent(t, st, src, tc.exception)
			}
			f := store.CalendarOccurrenceFilter{
				From: occurrenceTime(t, "2018-11-04T03:00:00Z"),
				To:   occurrenceTime(t, "2018-11-04T04:00:00Z"),
			}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
			requirements.NoError(err)
			wantCount := 1
			if tc.exception != nil {
				wantCount = 2 // The moved exception and the nominal occurrence overlap this window.
			}
			requirements.Len(page.Items, wantCount)
			wantRef := fmt.Sprintf("evt:%d@%s", id, tc.key)
			var want store.CalendarOccurrence
			found := false
			for _, item := range page.Items {
				if item.Ref == wantRef {
					want, found = item, true
					break
				}
			}
			requirements.True(found, "listed target ref %s", wantRef)
			assertions.Equal(occurrenceTime(t, "2018-11-04T03:00:00Z"), want.Start.UTC())
			assertions.Equal(occurrenceTime(t, "2018-11-05T02:00:00Z"), want.End.UTC())
			detail, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), want.Ref)
			requirements.NoError(err)
			assertions.Equal(want, detail)
		})
	}
}

func TestOccurrenceSourceDeletedReadable(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	id := putOccurrenceEvent(t, st, src, timedOccurrenceEvent("removed", "2027-03-01T10:00:00Z"))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?"), id)
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.True(page.Items[0].DeletedFromSource)
	got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), page.Items[0].Ref)
	requirements.NoError(err)
	assertions.True(got.DeletedFromSource)
}

func TestOccurrencesUnexpandableMore(t *testing.T) {
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	for i := range 51 {
		e := timedOccurrenceEvent(fmt.Sprintf("unsupported%d", i), "2027-03-01T10:00:00Z")
		e["recurrence"] = []string{"RRULE:FREQ=HOURLY"}
		putOccurrenceEvent(t, st, src, e)
	}
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	require.NoError(t, err)
	assertions.Empty(page.Items)
	assertions.Len(page.Unexpandable, 50)
	assertions.True(page.UnexpandableMore)
}

func TestGetOccurrenceManyExceptionsBounded(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("many-get", "2027-03-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=DAILY"}
	id := putOccurrenceEvent(t, st, src, master)
	// Another calendar's many exceptions must not broaden this source's read.
	other := occurrenceSource(t, st, "other@example.com")
	seedOccurrenceExceptions(t, st, other, id, "many-get", 20001)
	got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), fmt.Sprintf("evt:%d@20270301T100000Z", id))
	requirements.NoError(err)
	assertions.Equal(id, got.MessageID)
	// Move the rows into the same source. Now the bounded read must reject them.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET source_id=? WHERE source_id=?"), src, other)
	requirements.NoError(err)
	_, err = st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), fmt.Sprintf("evt:%d@20270301T100000Z", id))
	assertions.ErrorIs(err, store.ErrQueryTooBroad)
}

func TestOccurrenceZeroDurationListGet(t *testing.T) {
	for _, kind := range []string{"series", "exception"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			src := occurrenceSource(t, st, "owner@example.com")
			master := timedOccurrenceEvent("point", "2027-03-01T10:00:00Z")
			master["recurrence"] = []string{"RRULE:FREQ=WEEKLY;COUNT=2"}
			start := occurrenceTime(t, "2027-03-01T10:00:00Z")
			key := "20270301T100000Z"
			if kind == "series" {
				master["end"] = master["start"]
			}
			id := putOccurrenceEvent(t, st, src, master)
			if kind == "exception" {
				start = occurrenceTime(t, "2027-03-08T10:30:00Z")
				key = "20270308T100000Z"
				exception := timedOccurrenceEvent("point_20270308T100000Z", start.Format(time.RFC3339))
				exception["end"] = exception["start"]
				exception["recurringEventId"] = "point"
				exception["originalStartTime"] = map[string]any{"dateTime": "2027-03-08T10:00:00Z"}
				putOccurrenceEvent(t, st, src, exception)
			}
			f := store.CalendarOccurrenceFilter{From: start.Add(-time.Minute), To: start.Add(time.Minute)}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
			requirements.NoError(err)
			requirements.Len(page.Items, 1)
			assertions.Equal(fmt.Sprintf("evt:%d@%s", id, key), page.Items[0].Ref)
			got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), page.Items[0].Ref)
			requirements.NoError(err)
			assertions.Equal(page.Items[0], got)
			// Point events at the inclusive lower bound behave like standalone events.
			f.From = start
			page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
			requirements.NoError(err)
			requirements.Len(page.Items, 1)
			assertions.Equal(got, page.Items[0])
		})
	}
}

// seedOccurrenceExceptions exercises retrieval limits on real derived rows,
// without decoding thousands of identical synthetic provider payloads.
func seedOccurrenceExceptions(t *testing.T, st *store.Store, src, masterID int64, series string, count int) {
	t.Helper()
	conv, err := st.EnsureConversation(src, "exception-fixture", "Synthetic exceptions")
	require.NoError(t, err)
	tx, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	insert, err := tx.PrepareContext(t.Context(), st.Rebind("INSERT INTO messages (source_id,conversation_id,source_message_id,message_type) VALUES (?,?,?,'calendar_event') RETURNING id"))
	require.NoError(t, err)
	defer func() { _ = insert.Close() }()
	project, err := tx.PrepareContext(t.Context(), st.Rebind("INSERT INTO calendar_events (message_id,projection_version,content_hash,provider,namespace,provider_event_id,series_provider_id,kind,status) SELECT ?,projection_version,content_hash,provider,namespace,?,?,'exception','cancelled' FROM calendar_events WHERE message_id=?"))
	require.NoError(t, err)
	defer func() { _ = project.Close() }()
	for i := range count {
		provider := fmt.Sprintf("%s_%d", series, i)
		var mid int64
		require.NoError(t, insert.QueryRowContext(t.Context(), src, conv, provider).Scan(&mid))
		_, err = project.ExecContext(t.Context(), mid, provider, series, masterID)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

func TestOccurrenceFractionalDTStartUnsupported(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "fractional@example.com")
	event := timedOccurrenceEvent("fractional", "2027-03-01T10:00:00.123Z")
	event["end"] = map[string]any{"dateTime": "2027-03-01T11:00:00.123Z"}
	event["recurrence"] = []string{"RDATE:20270308T100000Z"}
	id := putOccurrenceEvent(t, st, src, event)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	requirements.NoError(err)
	assertions.Empty(page.Items)
	assertions.Equal([]store.UnexpandableSeries{{Ref: fmt.Sprintf("evt:%d", id), Reason: "unsupported"}}, page.Unexpandable)
	// Standalone instants retain their precision and do not use recurrence keys.
	delete(event, "recurrence")
	putOccurrenceEvent(t, st, src, event)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), occurrenceWindow(t))
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal(123000000, page.Items[0].Start.Nanosecond())
	got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), page.Items[0].Ref)
	requirements.NoError(err)
	assertions.Equal(page.Items[0], got)
}

func TestOccurrencesCancelledMasterOverridesConfirmedException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("cancelled-series", "2027-03-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=WEEKLY;COUNT=2"}
	id := putOccurrenceEvent(t, st, src, master)
	exception := timedOccurrenceEvent("cancelled-series_20270308T100000Z", "2027-03-08T12:00:00Z")
	exception["recurringEventId"] = "cancelled-series"
	exception["originalStartTime"] = map[string]any{"dateTime": "2027-03-08T10:00:00Z"}
	exceptionID := putOccurrenceEvent(t, st, src, exception)
	master["status"] = "cancelled"
	putOccurrenceEvent(t, st, src, master)
	f := occurrenceWindow(t)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	assertions.Empty(page.Items)
	f.IncludeCancelled = true
	page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
	requirements.NoError(err)
	requirements.Len(page.Items, 2)
	for _, o := range page.Items {
		assertions.True(o.Cancelled)
		assertions.Equal("cancelled", o.Status)
		got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), o.Ref)
		requirements.NoError(err)
		assertions.Equal(o, got)
	}
	assertions.Equal(exceptionID, page.Items[1].MessageID)
	assertions.Equal(id, page.Items[1].SeriesMessageID)
	assertions.Equal(occurrenceTime(t, "2027-03-08T12:00:00Z"), page.Items[1].Start.UTC())
}

func TestOccurrencesExceptionEffectiveTypeCurrentZone(t *testing.T) {
	for _, masterAllDay := range []bool{false, true} {
		t.Run(fmt.Sprintf("master_all_day_%t", masterAllDay), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			src := occurrenceSource(t, st, "owner@example.com")
			master := timedOccurrenceEvent("converted", "2027-03-01T10:00:00Z")
			master["start"] = map[string]any{"dateTime": "2027-03-01T10:00:00Z", "timeZone": "America/Los_Angeles"}
			master["recurrence"] = []string{"RRULE:FREQ=WEEKLY;COUNT=2"}
			original := map[string]any{"dateTime": "2027-03-08T10:00:00Z"}
			key := "20270308T100000Z"
			if masterAllDay {
				master["start"], master["end"] = map[string]any{"date": "2027-03-01"}, map[string]any{"date": "2027-03-02"}
				original, key = map[string]any{"date": "2027-03-08"}, "20270308"
			}
			id := putOccurrenceEvent(t, st, src, master)
			exception := timedOccurrenceEvent("converted_"+key, "2027-03-08T12:00:00Z")
			exception["recurringEventId"], exception["originalStartTime"] = "converted", original
			wantStart, wantEnd := "2027-03-08T12:00:00Z", "2027-03-08T13:00:00Z"
			if !masterAllDay {
				exception["start"], exception["end"] = map[string]any{"date": "2027-03-08"}, map[string]any{"date": "2027-03-09"}
				wantStart, wantEnd = "2027-03-07T10:00:00Z", "2027-03-08T10:00:00Z"
			}
			exceptionID := putOccurrenceEvent(t, st, src, exception)
			// Reads use the new source zone without rebuilding projection rows.
			requirements.NoError(st.UpdateSourceSyncConfig(src, `{"calendar_id":"primary","time_zone":"Pacific/Kiritimati"}`))
			start, end := occurrenceTime(t, wantStart), occurrenceTime(t, wantEnd)
			f := store.CalendarOccurrenceFilter{From: start, To: start.Add(time.Minute)}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
			requirements.NoError(err)
			requirements.Len(page.Items, 1)
			o := page.Items[0]
			assertions.Equal(exceptionID, o.MessageID)
			assertions.Equal(fmt.Sprintf("evt:%d@%s", id, key), o.Ref)
			assertions.Equal(masterAllDay, o.Original.AllDay)
			assertions.Equal(!masterAllDay, o.AllDay)
			assertions.Equal(start, o.Start.UTC())
			assertions.Equal(end, o.End.UTC())
			if masterAllDay {
				assertions.Empty(o.StartDate)
				assertions.Empty(o.EndDate)
			} else {
				assertions.Equal("2027-03-08", o.StartDate)
				assertions.Equal("2027-03-09", o.EndDate)
			}
			got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), o.Ref)
			requirements.NoError(err)
			assertions.Equal(o, got)
			f.From, f.To = end, end.Add(time.Minute)
			page, err = st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), f)
			requirements.NoError(err)
			assertions.Empty(page.Items)
		})
	}
}

func TestOccurrencesEarlierRDateListAndDetail(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "owner@example.com")
	master := timedOccurrenceEvent("early-rdate", "2027-06-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=DAILY;COUNT=1", "RDATE:20270302T100000Z"}
	id := putOccurrenceEvent(t, st, src, master)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), store.OwnerScope(), store.CalendarOccurrenceFilter{
		From: occurrenceTime(t, "2027-03-02T10:00:00Z"), To: occurrenceTime(t, "2027-03-02T11:00:00Z"),
	})
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270302T100000Z", id), page.Items[0].Ref)
	got, err := st.GetCalendarOccurrenceContext(t.Context(), store.OwnerScope(), page.Items[0].Ref)
	requirements.NoError(err)
	assertions.Equal(page.Items[0], got)
}
