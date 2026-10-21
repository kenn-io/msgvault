package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func calendarWindow(t *testing.T, st *Store, from, to string) ([]CalendarEventRow, error) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, from)
	require.NoError(t, err)
	end, err := time.Parse(time.RFC3339, to)
	require.NoError(t, err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	return calendarEventRowsInWindow(ctx, st.meetingReader(ctx), start, end)
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
				eventValues = append(eventValues, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
				eventArgs = append(eventArgs, id, 1, "fixture", providerID, seriesID, kind, date, "cancelled", true, "UTC", `[]`)
			}
			override := ""
			if st.IsPostgreSQL() {
				override = " OVERRIDING SYSTEM VALUE"
			}
			if _, err := tx.Exec(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type)`+override+` VALUES `+strings.Join(messageValues, ","), messageArgs...); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO calendar_events (message_id,projection_version,content_hash,provider_event_id,series_provider_id,kind,original_start_date,status,all_day,calendar_time_zone,recurrence_json) VALUES `+strings.Join(eventValues, ","), eventArgs...); err != nil {
				return err
			}
		}
		return nil
	}))
}

func TestCalendarEventRowsManyExceptionsBounded(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", `{"id":"cap-series","start":{"date":"1970-01-01"},"end":{"date":"1970-01-02"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "exception", 20001)
	rows, err := calendarWindow(t, st, "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	requirements.NoError(err)
	assertions.Len(rows, 1)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Len(page.Occurrences, 1)
	// The same budget still rejects exceptions that actually affect the window.
	_, err = st.ListCalendarOccurrencesContext(t.Context(), time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	requirements.ErrorIs(err, ErrQueryTooBroad)
	assertions.Contains(err.Error(), "narrow the window")
}

func TestCalendarEventRowsHardSeriesCap(t *testing.T) {
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", calendarRaw)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", 2001)
	_, err := st.db.Exec(`UPDATE calendar_events SET start_at = '2027-03-01 00:00:00+00:00', end_at = '2027-03-02 00:00:00+00:00', start_date = '2027-03-01', end_date = '2027-03-02', recurrence_json = '["RRULE:FREQ=DAILY"]' WHERE kind = 'series'`)
	require.NoError(t, err)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	for _, width := range []time.Duration{24 * time.Hour, time.Hour} {
		_, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(width))
		require.ErrorIs(t, err, ErrQueryTooBroad)
		assertions.Contains(err.Error(), "more than 2000 recurring series overlap the window")
		assertions.Contains(err.Error(), "a narrower window will not reduce open-ended series")
		assertions.NotContains(err.Error(), "narrow the window")
	}
}

func TestCalendarEventRowsBudgetRejectsBeforeDecoding(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "one", calendarRaw)
	calendarFixture(t, st, "two", calendarRaw)
	_, err := st.db.Exec(`UPDATE calendar_events SET recurrence_json = 'invalid'`)
	require.NoError(t, err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	_, err = queryCalendarEventRowsWithinBudget(ctx, st.meetingReader(ctx), "1 = 1", nil, 1)
	assert.ErrorIs(t, err, ErrQueryTooBroad)
}
func TestCalendarEventSourceZoneRebuild(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	data, id := calendarFixture(t, st, "zone", `{"id":"all-day","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"time_zone":"America/Los_Angeles"}`))
	row := calendarRows(t, st, id)[0]
	requirements.NotNil(row.Event.Start)
	assertions.Equal("2027-03-14T08:00:00Z", row.Event.Start.Format(time.RFC3339))
	dest, err := st.GetOrCreateSource("gcal", "destination@example.com")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(dest.ID, `{"time_zone":"Asia/Tokyo"}`))
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: data.Message.SourceID, IntoSourceID: dest.ID})
	requirements.NoError(err)
	// The merged copy is archive-only, so inspect its stored projection directly.
	var sourceID int64
	var start time.Time
	requirements.NoError(st.db.QueryRow(`SELECT m.source_id, e.start_at FROM calendar_events e JOIN messages m ON m.id = e.message_id WHERE e.message_id = ?`, id).Scan(&sourceID, &start))
	assertions.Equal(dest.ID, sourceID)
	assertions.Equal("2027-03-13T15:00:00Z", start.UTC().Format(time.RFC3339))
}

func TestCalendarExceptionsOriginalAndEffectiveWindows(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-02T09:00:00Z"},"start":{"dateTime":"2027-04-02T09:00:00Z"},"end":{"dateTime":"2027-04-02T10:00:00Z"}}`)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences, "the original occurrence moved outside the window")
	from = from.AddDate(0, 1, 0)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(moved, page.Occurrences[0].MessageID)
	assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
}

func TestCalendarExceptionSnapshotFreshnessPrecedesWindowFiltering(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	masterRaw := `{"id":"weekly","iCalUID":"weekly@example.test","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`
	_, master := calendarFixture(t, st, "master", masterRaw)
	aliasA := `{"id":"alias-a","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-02T09:00:00","timeZone":"UTC"},"start":{"dateTime":"2027-04-02T09:00:00Z"},"end":{"dateTime":"2027-04-02T10:00:00Z"}}`
	aliasB := `{"id":"alias-b","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-02T09:00:00Z"},"start":{"dateTime":"2027-05-02T09:00:00Z"},"end":{"dateTime":"2027-05-02T10:00:00Z"}}`
	_, olderID := calendarFixture(t, st, "alias-a", aliasA)
	_, newerID := calendarFixture(t, st, "alias-b", aliasB)
	// Fix the fixture watermarks so the higher-ID alias is initially newer,
	// then verify a raw-only reimport of the lower-ID alias advances freshness.
	_, err := st.db.Exec(`UPDATE messages SET content_changed_at = ? WHERE id = ?`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), olderID)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET content_changed_at = ? WHERE id = ?`, time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC), newerID)
	requirements.NoError(err)

	april := time.Date(2027, 4, 2, 0, 0, 0, 0, time.UTC)
	may := time.Date(2027, 5, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), april, april.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences, "the older April alias must not survive the newer May alias")
	page, err = st.ListCalendarOccurrencesContext(t.Context(), may, may.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(newerID, page.Occurrences[0].MessageID)

	var oldHash string
	requirements.NoError(st.db.QueryRow(`SELECT content_hash FROM calendar_events WHERE message_id = ?`, olderID).Scan(&oldHash))
	var oldFreshness time.Time
	requirements.NoError(st.db.QueryRow(`SELECT content_changed_at FROM messages WHERE id = ?`, olderID).Scan(&oldFreshness))
	refreshedA := strings.Replace(aliasA, `"originalStartTime"`, `"updated":"2027-03-10T00:00:00Z","originalStartTime"`, 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(olderID, []byte(refreshedA), "gcal_json"))
	var refreshedHash string
	requirements.NoError(st.db.QueryRow(`SELECT content_hash FROM calendar_events WHERE message_id = ?`, olderID).Scan(&refreshedHash))
	assertions.Equal(oldHash, refreshedHash, "provider metadata outside the projection must not alter its content hash")
	var refreshedFreshness time.Time
	requirements.NoError(st.db.QueryRow(`SELECT content_changed_at FROM messages WHERE id = ?`, olderID).Scan(&refreshedFreshness))
	assertions.True(refreshedFreshness.After(oldFreshness), "raw snapshot updates must advance freshness even when the projection is unchanged")

	page, err = st.ListCalendarOccurrencesContext(t.Context(), april, april.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(olderID, page.Occurrences[0].MessageID)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), may, may.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences, "the fresher April snapshot must suppress the stale May alias")

	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	budget := calrecur.RequestReadBudget
	resolved, err := resolveMeetingOccurrence(ctx, st.meetingReader(ctx), OccurrenceRef{MessageID: master, Key: "20270302T090000Z"}, &budget)
	release()
	requirements.NoError(err)
	assertions.Equal(olderID, resolved.Row.MessageID, "saved occurrence references must resolve through the freshest alias")

	cancelledA := `{"id":"alias-a","updated":"2027-03-11T00:00:00Z","status":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-02T09:00:00","timeZone":"UTC"}}`
	requirements.NoError(st.UpsertMessageRawWithFormat(olderID, []byte(cancelledA), "gcal_json"))
	original := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), original, original.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(olderID, page.Occurrences[0].MessageID)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
}

func TestCalendarExceptionsPartialBounds(t *testing.T) {
	for _, tc := range []struct {
		name, original, bounds, start, end string
	}{
		{"end only", "2027-03-02T09:00:00Z", `"end":{"dateTime":"2027-04-03T10:00:00Z"}`, "2027-03-02T09:00:00Z", "2027-04-03T10:00:00Z"},
		{"start only", "2027-05-02T09:00:00Z", `"start":{"dateTime":"2027-04-01T09:00:00Z"}`, "2027-04-01T09:00:00Z", "2027-05-02T10:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"weekly","start":{"dateTime":%q},"end":{"dateTime":%q},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=1"]}`, tc.original, tc.original[:11]+"10:00:00Z"))
			_, exception := calendarFixture(t, st, "partial", fmt.Sprintf(`{"id":"partial","recurringEventId":"weekly","originalStartTime":{"dateTime":%q},%s}`, tc.original, tc.bounds))
			from := time.Date(2027, 4, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			assertions.Empty(page.Unexpandable)
			requirements.Len(page.Occurrences, 1)
			o := page.Occurrences[0]
			assertions.Equal(exception, o.MessageID)
			assertions.Equal(master, o.SeriesMessageID)
			assertions.Equal(tc.start, o.Start.Format(time.RFC3339))
			assertions.Equal(tc.end, o.End.Format(time.RFC3339))
		})
	}
}

func TestCalendarListingIgnoresUnrelatedMetadata(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "metadata@example.org"))
	metadata := `{"ical_uid":"metadata@example.org","large_field":"` + strings.Repeat("x", 10000) + `"}`
	requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: metadata, Valid: true}))
	assertions.Equal("metadata@example.org", calendarRows(t, st, event)[0].ICalUID)
	// JSONB rejects malformed input at storage time.
	if st.IsPostgreSQL() {
		return
	}
	// Simulate malformed metadata archived before the UID index was introduced.
	_, err := st.db.Exec(`DROP INDEX idx_messages_calendar_ical_uid`)
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"broken":`, Valid: true}))
	requirements.NoError(st.InitSchemaContext(t.Context()), "schema setup must accept malformed legacy metadata")
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(24*time.Hour))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(event, page.Occurrences[0].MessageID)
}

func TestCalendarExceptionsShortenedSeriesDuration(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, original, recurrence string
	}{
		{"timed count", `{"dateTime":"2027-03-02T09:00:00Z"}`, `{"dateTime":"2027-03-05T09:00:00Z"}`, `{"dateTime":"2027-03-09T09:00:00Z"}`, "RRULE:FREQ=WEEKLY;COUNT=1"},
		{"all-day until", `{"date":"2027-03-02"}`, `{"date":"2027-03-06"}`, `{"date":"2027-03-09"}`, "RRULE:FREQ=WEEKLY;UNTIL=20270302"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := fmt.Sprintf(`{"id":"weekly","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`, tc.start, tc.end)
			_, master := calendarFixture(t, st, "master", raw)
			_, cancelled := calendarFixture(t, st, "cancelled", fmt.Sprintf(`{"id":"cancelled","recurringEventId":"weekly","originalStartTime":%s,"status":"cancelled"}`, tc.original))
			shortened := strings.Replace(raw, "RRULE:FREQ=WEEKLY;COUNT=3", tc.recurrence, 1)
			require.NoError(st.UpsertMessageRawWithFormat(master, []byte(shortened), "gcal_json"))
			from := time.Date(2027, 3, 12, 0, 0, 0, 0, time.UTC)
			wide, err := st.ListCalendarOccurrencesContext(t.Context(), from.AddDate(0, 0, -3), from.AddDate(0, 0, 1))
			require.NoError(err)
			require.Len(wide.Occurrences, 1)
			assert.Equal(cancelled, wide.Occurrences[0].MessageID)
			assert.Equal(master, wide.Occurrences[0].SeriesMessageID)
			assert.Equal("cancelled", wide.Occurrences[0].Status)
			narrow, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			require.NoError(err)
			assert.Empty(narrow.Unexpandable)
			assert.Equal(wide.Occurrences, narrow.Occurrences)
		})
	}
}

func TestCalendarEventRowsEndedSeriesOutsideWindow(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "current", `{"id":"current","start":{"dateTime":"2027-03-12T09:00:00Z"},"end":{"dateTime":"2027-03-12T10:00:00Z"}}`)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", 2001)
	_, err := st.db.Exec(`UPDATE calendar_events SET start_at = '2027-03-01 00:00:00+00:00', end_at = '2027-03-02 00:00:00+00:00', start_date = '2027-03-01', end_date = '2027-03-02', recurrence_end_at = '2027-03-02 00:00:00+00:00', recurrence_json = '["RRULE:FREQ=DAILY;COUNT=1"]' WHERE kind = 'series'`)
	require.NoError(err)
	from := time.Date(2027, 3, 12, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	require.NoError(err)
	assert.Empty(page.Unexpandable)
	require.Len(page.Occurrences, 1)
	assert.Equal(event, page.Occurrences[0].MessageID)
}

func TestCalendarExceptionsClearedRecurrence(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, original, wantStart, wantEnd string
	}{
		{"timed", `{"dateTime":"2027-03-02T09:00:00Z"}`, `{"dateTime":"2027-03-05T09:00:00Z"}`, `{"dateTime":"2027-03-09T09:00:00Z"}`, "2027-03-09T09:00:00Z", "2027-03-12T09:00:00Z"},
		{"all-day", `{"date":"2027-03-02"}`, `{"date":"2027-03-06"}`, `{"date":"2027-03-09"}`, "2027-03-09T00:00:00Z", "2027-03-13T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			raw := fmt.Sprintf(`{"id":"weekly","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`, tc.start, tc.end)
			_, master := calendarFixture(t, st, "master", raw)
			_, cancelled := calendarFixture(t, st, "cancelled", fmt.Sprintf(`{"id":"cancelled","recurringEventId":"weekly","originalStartTime":%s,"status":"cancelled"}`, tc.original))
			cleared := strings.Replace(raw, `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]`, "", 1)
			require.NoError(t, st.UpsertMessageRawWithFormat(master, []byte(cleared), "gcal_json"))
			for _, window := range []struct {
				name       string
				day, count int
			}{
				{"whole month", 1, 2},
				{"original start", 9, 1},
				{"inside cancellation", 12, 1},
			} {
				t.Run(window.name, func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					from := time.Date(2027, 3, window.day, 0, 0, 0, 0, time.UTC)
					to := from.AddDate(0, 0, 1)
					if window.day == 1 {
						to = from.AddDate(0, 1, 0)
					}
					page, err := st.ListCalendarOccurrencesContext(t.Context(), from, to)
					require.NoError(err)
					assert.Empty(page.Unexpandable)
					require.Len(page.Occurrences, window.count)
					if window.count == 2 {
						assert.Equal(master, page.Occurrences[0].MessageID)
						assert.Zero(page.Occurrences[0].SeriesMessageID)
					}
					o := page.Occurrences[window.count-1]
					assert.Equal(cancelled, o.MessageID)
					assert.Equal(master, o.SeriesMessageID)
					assert.Equal("cancelled", o.Status)
					assert.Equal("Example event", o.Subject)
					assert.Equal(tc.wantStart, o.Start.Format(time.RFC3339))
					assert.Equal(tc.wantEnd, o.End.Format(time.RFC3339))
				})
			}
		})
	}
}

func TestCalendarClearedMasterKeepsStandaloneAndFirstOccurrenceExceptions(t *testing.T) {
	for _, tc := range []struct {
		name, exception, effectiveStart, effectiveEnd, status string
		originalCount, effectiveCount                         int
	}{
		{
			name:           "cancelled DTSTART",
			exception:      `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-02T09:00:00Z"},"status":"cancelled"}`,
			effectiveStart: "2027-03-02T09:00:00Z",
			effectiveEnd:   "2027-03-02T10:00:00Z",
			status:         "cancelled",
			originalCount:  1,
		},
		{
			name:           "moved DTSTART",
			exception:      `{"id":"moved","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-02T09:00:00Z"},"start":{"dateTime":"2027-03-03T09:00:00Z"},"end":{"dateTime":"2027-03-03T10:00:00Z"}}`,
			effectiveStart: "2027-03-03T09:00:00Z",
			effectiveEnd:   "2027-03-03T10:00:00Z",
			status:         "confirmed",
			effectiveCount: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"weekly","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
			_, master := calendarFixture(t, st, "master", raw)
			_, exception := calendarFixture(t, st, "exception", tc.exception)
			cleared := strings.Replace(raw, `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]`, "", 1)
			requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(cleared), "gcal_json"))

			for _, window := range []struct {
				name string
				day  int
				want int
			}{
				{name: "original date", day: 2, want: tc.originalCount},
				{name: "effective date", day: 3, want: tc.effectiveCount},
			} {
				t.Run(window.name, func(t *testing.T) {
					from := time.Date(2027, 3, window.day, 0, 0, 0, 0, time.UTC)
					page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
					require.NoError(t, err)
					if window.day == 2 {
						require.Len(t, page.Occurrences, window.want+1)
						standalone := page.Occurrences[0]
						assertions.Equal(master, standalone.MessageID)
						assertions.Zero(standalone.SeriesMessageID)
						assertions.Equal("confirmed", standalone.Status)
						assertions.Equal("2027-03-02T09:00:00Z", standalone.Start.Format(time.RFC3339))
						page.Occurrences = page.Occurrences[1:]
					}
					require.Len(t, page.Occurrences, window.want)
					if window.want == 0 {
						return
					}
					occurrence := page.Occurrences[0]
					assertions.Equal(exception, occurrence.MessageID)
					assertions.Equal(master, occurrence.SeriesMessageID)
					assertions.Equal(tc.status, occurrence.Status)
					assertions.Equal(tc.effectiveStart, occurrence.Start.Format(time.RFC3339))
					assertions.Equal(tc.effectiveEnd, occurrence.End.Format(time.RFC3339))
				})
			}
		})
	}
}
