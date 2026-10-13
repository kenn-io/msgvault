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
