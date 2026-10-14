package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/calrecur"
)

func calendarWindow(t *testing.T, st *Store, scope SourceScope, from, to string) ([]CalendarEventRow, error) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, from)
	require.NoError(t, err)
	end, err := time.Parse(time.RFC3339, to)
	require.NoError(t, err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	return calendarEventRowsInWindow(ctx, st.snapshotReader(ctx), scope, start, end)
}

func TestCalendarEventRowsWindowFetchesMasterOfMovedException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"series-example","start":{"dateTime":"2027-03-01T17:00:00Z"},"end":{"dateTime":"2027-03-01T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`)
	_, exception := calendarFixture(t, st, "exception", `{"id":"series-example_20270301T170000Z","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-01T17:00:00Z"},"start":{"dateTime":"2027-03-20T17:00:00Z"},"end":{"dateTime":"2027-03-20T18:00:00Z"}}`)
	rows, err := calendarWindow(t, st, OwnerScope(), "2027-03-20T00:00:00Z", "2027-03-21T00:00:00Z")
	requirements.NoError(err)
	requirements.Len(rows, 2)
	assertions.Equal([]int64{master, exception}, []int64{rows[0].MessageID, rows[1].MessageID})
	// Local scope applies before fetching parents or their exceptions.
	rows, err = calendarWindow(t, st, SourceScope{}, "2027-03-20T00:00:00Z", "2027-03-21T00:00:00Z")
	requirements.NoError(err)
	assertions.Empty(rows)
}

func TestCalendarEventRowsWindowIncludesEarlierRDate(t *testing.T) {
	for _, tc := range []struct {
		name, bounds, recurrence, wantStart, wantEnd string
		allDay                                       bool
	}{
		{"UTC", `"start":{"dateTime":"2027-06-01T17:00:00Z"},"end":{"dateTime":"2027-06-01T18:00:00Z"}`, `"RRULE:FREQ=DAILY;COUNT=1","RDATE:20270302T170000Z"`, "2027-03-02T17:00:00Z", "2027-03-02T18:00:00Z", false},
		{"TZID", `"start":{"dateTime":"2027-06-01T09:00:00-07:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-06-01T10:00:00-07:00"}`, `"RDATE;TZID=America/Los_Angeles:20270302T090000"`, "2027-03-02T17:00:00Z", "2027-03-02T18:00:00Z", false},
		{"floating", `"start":{"dateTime":"2027-06-01T17:00:00Z"},"end":{"dateTime":"2027-06-01T18:00:00Z"}`, `"RDATE:20270302T170000"`, "2027-03-02T17:00:00Z", "2027-03-02T18:00:00Z", false},
		{"DATE", `"start":{"date":"2027-06-01"},"end":{"date":"2027-06-02"}`, `"RRULE:FREQ=DAILY;COUNT=1","RDATE;VALUE=DATE:20270302"`, "2027-03-02T00:00:00Z", "2027-03-03T00:00:00Z", true},
		{"RDATE_only", `"start":{"dateTime":"2027-06-01T17:00:00Z"},"end":{"dateTime":"2027-06-01T18:00:00Z"}`, `"RDATE:20270302T170000Z"`, "2027-03-02T17:00:00Z", "2027-03-02T18:00:00Z", false},
		{"escaped_JSON", `"start":{"dateTime":"2027-06-01T17:00:00Z"},"end":{"dateTime":"2027-06-01T18:00:00Z"}`, `"\u0052DATE:20270302T170000Z"`, "2027-03-02T17:00:00Z", "2027-03-02T18:00:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, master := calendarFixture(t, st, "earlier-rdate", `{"id":"earlier-rdate",`+tc.bounds+`,"recurrence":[`+tc.recurrence+`]}`)
			calendarFixture(t, st, "future-rrule", `{"id":"future-rrule",`+tc.bounds+`,"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`)
			rows, err := calendarWindow(t, st, SourceScope{IDs: []int64{data.Message.SourceID}}, "2027-03-02T00:00:00Z", "2027-03-03T00:00:00Z")
			requirements.NoError(err)
			requirements.Len(rows, 1)
			assertions.Equal(master, rows[0].MessageID)
			event := rows[0].Event
			requirements.Empty(rows[0].RecurrenceError)
			requirements.NotNil(event.Start)
			requirements.NotNil(event.End)
			zone := time.UTC
			if event.EventTimeZone != "" {
				zone, err = time.LoadLocation(event.EventTimeZone)
				requirements.NoError(err)
			}
			set, err := calrecur.ParseSet(*event.Start, event.AllDay, event.StartDate, zone, event.Recurrence)
			requirements.NoError(err)
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			occurrences, err := calrecur.Expand(t.Context(), set, event.End.Sub(*event.Start), 1, nil, from, from.AddDate(0, 0, 1), calrecur.SeriesReadBudget)
			requirements.NoError(err)
			requirements.Len(occurrences, 1)
			assertions.Equal(tc.wantStart, occurrences[0].Start.UTC().Format(time.RFC3339))
			assertions.Equal(tc.wantEnd, occurrences[0].End.UTC().Format(time.RFC3339))
			assertions.Equal(tc.allDay, occurrences[0].AllDay)
			rows, err = calendarWindow(t, st, SourceScope{}, "2027-03-02T00:00:00Z", "2027-03-03T00:00:00Z")
			requirements.NoError(err)
			assertions.Empty(rows)
		})
	}
}

func FuzzCalendarEventRowsEarlierRDate(f *testing.F) {
	f.Add(uint8(90), false)
	f.Add(uint8(90), true)
	f.Fuzz(func(t *testing.T, gap uint8, allDay bool) {
		st := newRFC822IDBackfillBackendStore(t)
		rdate := time.Date(2027, 3, 2, 17, 0, 0, 0, time.UTC)
		start := rdate.AddDate(0, 0, int(gap)+3)
		bounds := fmt.Sprintf(`"start":{"dateTime":%q},"end":{"dateTime":%q}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
		line := "RDATE:" + rdate.Format("20060102T150405Z")
		if allDay {
			bounds = fmt.Sprintf(`"start":{"date":%q},"end":{"date":%q}`, start.Format(time.DateOnly), start.AddDate(0, 0, 1).Format(time.DateOnly))
			line = "RDATE;VALUE=DATE:" + rdate.Format("20060102")
		}
		_, master := calendarFixture(t, st, "earlier-rdate", fmt.Sprintf(`{"id":"earlier-rdate",%s,"recurrence":[%q]}`, bounds, line))
		rows, err := calendarWindow(t, st, OwnerScope(), "2027-03-02T00:00:00Z", "2027-03-03T00:00:00Z")
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, master, rows[0].MessageID)
	})
}

func TestCalendarEventRowsTimezoneChangeWithoutResync(t *testing.T) {
	for _, tt := range []struct{ before, after, from, to string }{
		{"UTC", "America/Los_Angeles", "2027-03-15T03:00:00Z", "2027-03-15T04:00:00Z"},
		{"Pacific/Kiritimati", "Pacific/Honolulu", "2027-03-15T09:00:00Z", "2027-03-15T09:30:00Z"},
	} {
		t.Run(tt.before+"_"+tt.after, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, id := calendarFixture(t, st, "zone-change", `{"id":"all-day-example","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
			requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"`+tt.before+`"}`))
			_, err := st.PersistMessage(data)
			requirements.NoError(err)
			before := calendarRows(t, st, id)[0].Event.Start
			requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"`+tt.after+`"}`))
			rows, err := calendarWindow(t, st, OwnerScope(), tt.from, tt.to)
			requirements.NoError(err)
			requirements.Len(rows, 1)
			assertions.Equal(id, rows[0].MessageID)
			assertions.Equal(before, rows[0].Event.Start, "zone changes do not rewrite projected bounds")
			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			requirements.NoError(err)
			defer release()
			zone, err := currentCalendarZone(ctx, st.snapshotReader(ctx), data.Message.SourceID)
			requirements.NoError(err)
			assertions.Equal(tt.after, zone.String())
		})
	}
}

// The v3.1 gate replaces paging with an explicit error at the series cap.
func TestCalendarEventRowsPagingBySeries(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for _, id := range []string{"first-series", "second-series"} {
		calendarFixture(t, st, id, `{"id":"`+id+`","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY"]}`)
	}
	rows, err := calendarWindow(t, st, OwnerScope(), "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	requirements.NoError(err)
	requirements.Len(rows, 2)
}

func TestCalendarEventRowsExcludeLocallyDeleted(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, local := calendarFixture(t, st, "local-delete", calendarRaw)
	_, sourceDeleted := calendarFixture(t, st, "source-delete", calendarRaw)
	_, err := st.db.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, local)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, sourceDeleted)
	requirements.NoError(err)
	rows := calendarRows(t, st, local, sourceDeleted)
	requirements.Len(rows, 1)
	assertions.Equal(sourceDeleted, rows[0].MessageID)
	assertions.True(rows[0].DeletedFromSource)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	for _, lookup := range []func(context.Context, rowReader, SourceScope) ([]CalendarEventRow, error){
		func(ctx context.Context, r rowReader, sc SourceScope) ([]CalendarEventRow, error) {
			return calendarEventRowsByProviderID(ctx, r, sc, "google", "calendar@example.com", "event-example")
		},
		func(ctx context.Context, r rowReader, sc SourceScope) ([]CalendarEventRow, error) {
			return calendarEventRowsByICalUID(ctx, r, sc, "event-uid@example.com")
		},
	} {
		got, err := lookup(ctx, st.snapshotReader(ctx), OwnerScope())
		requirements.NoError(err)
		requirements.Len(got, 1)
		assertions.Equal(sourceDeleted, got[0].MessageID)
		got, err = lookup(ctx, st.snapshotReader(ctx), SourceScope{})
		requirements.NoError(err)
		assertions.Empty(got)
	}
}

func TestOrphanExceptionRefAliasAfterMasterArrives(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, exception := calendarFixture(t, st, "orphan", `{"id":"series-example_20270314","recurringEventId":"series-example","originalStartTime":{"date":"2027-03-14"},"status":"cancelled"}`)
	readAlias := func(sc SourceScope) (OccurrenceRef, error) {
		ctx, release, err := st.BeginReadSnapshotContext(t.Context())
		requirements.NoError(err)
		defer release()
		return resolveOccurrenceRefAlias(ctx, st.snapshotReader(ctx), sc, OccurrenceRef{MessageID: exception})
	}
	got, err := readAlias(OwnerScope())
	requirements.NoError(err)
	assertions.Equal(OccurrenceRef{MessageID: exception}, got)
	_, master := calendarFixture(t, st, "late-master", `{"id":"series-example","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY"]}`)
	got, err = readAlias(SourceScope{IDs: []int64{data.Message.SourceID}})
	requirements.NoError(err)
	assertions.Equal(OccurrenceRef{MessageID: master, Key: "20270314"}, got)
	_, err = readAlias(SourceScope{})
	assertions.ErrorIs(err, sql.ErrNoRows)
}

func TestCalendarEventLookupHelpers(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, master := calendarFixture(t, st, "lookup-master", `{"id":"lookup-series","iCalUID":"lookup@example.com","hangoutLink":"https://meet.google.com/abc-defg-hij","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`)
	_, exception := calendarFixture(t, st, "lookup-exception", `{"id":"lookup-series_20270315","recurringEventId":"lookup-series","iCalUID":"lookup@example.com","hangoutLink":"https://meet.google.com/abc-defg-hij","originalStartTime":{"date":"2027-03-15"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	r := st.snapshotReader(ctx)
	scoped := SourceScope{IDs: []int64{data.Message.SourceID}}
	for _, lookup := range []func(SourceScope) ([]CalendarEventRow, error){
		func(sc SourceScope) ([]CalendarEventRow, error) {
			return calendarEventRowsBySeriesProviderID(ctx, r, sc, "google", "calendar@example.com", "lookup-series")
		},
		func(sc SourceScope) ([]CalendarEventRow, error) {
			return calendarEventRowsByICalUID(ctx, r, sc, "lookup@example.com")
		},
		func(sc SourceScope) ([]CalendarEventRow, error) {
			return calendarEventRowsByJoinKey(ctx, r, sc, "meet:abc-defg-hij")
		},
	} {
		rows, err := lookup(scoped)
		requirements.NoError(err)
		requirements.Len(rows, 2)
		assertions.Equal([]int64{master, exception}, []int64{rows[0].MessageID, rows[1].MessageID})
		rows, err = lookup(SourceScope{})
		requirements.NoError(err)
		assertions.Empty(rows)
	}
	rows, err := calendarEventRowsByProviderID(ctx, r, scoped, "google", "wrong@example.com", "lookup-series")
	requirements.NoError(err)
	assertions.Empty(rows)
	rows, err = calendarEventRowsByProviderID(ctx, r, scoped, "google", "", "lookup-series")
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(master, rows[0].MessageID)
}

func bulkCalendarCandidates(t *testing.T, st *Store, sourceID, conversationID int64, kind string, count int) {
	t.Helper()
	require.NoError(t, st.withTxContext(t.Context(), func(tx *loggedTx) error {
		for offset := 0; offset < count; offset += 500 {
			size := min(500, count-offset)
			messageValues, eventValues := make([]string, 0, size), make([]string, 0, size)
			messageArgs, eventArgs := make([]any, 0, size*5), make([]any, 0, size*14)
			for i := range size {
				id := int64(100000 + offset + i)
				providerID := fmt.Sprintf("bulk-%s-%d", kind, id)
				seriesID := providerID
				if kind == "exception" {
					seriesID = "cap-series"
				}
				date := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, offset+i).Format(time.DateOnly)
				if kind != "exception" {
					date = ""
				}
				messageValues = append(messageValues, "(?, ?, ?, ?, ?)")
				messageArgs = append(messageArgs, id, sourceID, conversationID, providerID, "calendar_event")
				eventValues = append(eventValues, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
				eventArgs = append(eventArgs, id, 1, "fixture", "google", "calendar@example.com", providerID, seriesID, kind, date, "cancelled", true, "UTC", `[]`, false)
			}
			override := ""
			if st.IsPostgreSQL() {
				override = " OVERRIDING SYSTEM VALUE"
			}
			if _, err := tx.Exec(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type)`+override+` VALUES `+strings.Join(messageValues, ","), messageArgs...); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO calendar_events (message_id,projection_version,content_hash,provider,namespace,provider_event_id,series_provider_id,kind,original_start_date,status,all_day,calendar_time_zone,recurrence_json,has_bounds) VALUES `+strings.Join(eventValues, ","), eventArgs...); err != nil {
				return err
			}
		}
		return nil
	}))
}

func TestCalendarEventRowsManyExceptionsBounded(t *testing.T) {
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", `{"id":"cap-series","start":{"date":"1970-01-01"},"end":{"date":"1970-01-02"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "exception", 20001)
	rows, err := calendarWindow(t, st, OwnerScope(), "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	require.ErrorIs(t, err, ErrQueryTooBroad)
	assertions.Empty(rows)
}

func TestCalendarEventRowsHardSeriesCap(t *testing.T) {
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", calendarRaw)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", 2001)
	rows, err := calendarWindow(t, st, OwnerScope(), "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	require.ErrorIs(t, err, ErrQueryTooBroad)
	assertions.Empty(rows)
}

func TestCalendarEventRowsBudgetRejectsBeforeDecoding(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, first := calendarFixture(t, st, "budget-first", calendarRaw)
	// End equality belongs to the widened candidate window.
	lower := time.Date(2027, 3, 2, 18, 0, 0, 0, time.UTC)
	upper := lower.Add(time.Hour)
	query := func(ctx context.Context, scope SourceScope) ([]CalendarEventRow, error) {
		readCtx, release, err := st.BeginReadSnapshotContext(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		return queryCalendarEventRowsWithinBudget(readCtx, st.snapshotReader(readCtx), scope,
			`e.kind = 'single' AND e.start_at < ? AND e.end_at >= ?`, []any{upper, lower}, 2)
	}
	scope := SourceScope{IDs: []int64{data.Message.SourceID, data.Message.SourceID}}
	rows, err := query(t.Context(), scope)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(first, rows[0].MessageID)
	_, second := calendarFixture(t, st, "budget-second", calendarRaw)
	rows, err = query(t.Context(), scope)
	requirements.NoError(err)
	requirements.Len(rows, 2)
	assertions.Equal([]int64{first, second}, []int64{rows[0].MessageID, rows[1].MessageID})
	assertions.Equal("Raw description", rows[0].Event.Description)
	requirements.Len(rows[0].Event.Attendees, 1)
	assertions.Equal("person@example.com", rows[0].Event.Attendees[0].Email)

	_, third := calendarFixture(t, st, "budget-third", calendarRaw)
	_, err = st.db.Exec(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, third)
	requirements.NoError(err, "source-deleted evidence still consumes budget")
	// A breadth rejection must happen before payload decoding. This malformed
	// row makes the old materialize-first path fail for a different reason.
	_, err = st.db.Exec(`UPDATE calendar_events SET recurrence_json = '{' WHERE message_id = ?`, third)
	requirements.NoError(err)
	rows, err = query(t.Context(), scope)
	requirements.ErrorIs(err, ErrQueryTooBroad)
	assertions.Empty(rows)
	for _, hidden := range []SourceScope{{}, {IDs: []int64{0, -1}}, {IDs: []int64{data.Message.SourceID + 1000}}} {
		rows, err = query(t.Context(), hidden)
		requirements.NoError(err)
		assertions.Empty(rows)
	}

	for _, update := range []string{
		`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`,
		`UPDATE calendar_events SET kind = 'series' WHERE message_id = ?`,
		`UPDATE calendar_events SET kind = 'single', start_at = NULL WHERE message_id = ?`,
	} {
		_, err = st.db.Exec(`UPDATE messages SET deleted_at = NULL WHERE id = ?`, third)
		requirements.NoError(err)
		_, err = st.db.Exec(update, third)
		requirements.NoError(err)
		rows, err = query(t.Context(), scope)
		requirements.NoError(err)
		requirements.Len(rows, 2)
		assertions.Equal([]int64{first, second}, []int64{rows[0].MessageID, rows[1].MessageID})
	}
	for _, bounds := range [][2]time.Time{
		{upper, upper.Add(time.Hour)},
		{lower.Add(-time.Hour), lower.Add(-time.Nanosecond)},
	} {
		_, err = st.db.Exec(`UPDATE calendar_events SET start_at = ?, end_at = ? WHERE message_id = ?`, bounds[0], bounds[1], third)
		requirements.NoError(err)
		rows, err = query(t.Context(), scope)
		requirements.NoError(err)
		requirements.Len(rows, 2)
		assertions.Equal([]int64{first, second}, []int64{rows[0].MessageID, rows[1].MessageID})
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = queryCalendarEventRowsWithinBudget(cancelled, st.snapshotReader(t.Context()), SourceScope{}, "1 = 1", nil, 2)
	requirements.ErrorIs(err, context.Canceled)
	assertions.Empty(rows)
}
