package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingSchemaPlainSQLiteIntegrity(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := OpenForTest(path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.db.Close()) })
	require.NoError(st.InitSchemaContext(t.Context()))
	projectionFixture(t, st, "scheduled", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T06:30:00-04:00"}}`)
	_, note := projectionFixture(t, st, "recording", "plaud_json", `{}`)
	_, err = st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, "2027-03-14T06:30:00-04:00", note)
	require.NoError(err)
	require.NoError(st.Close())

	// Backup restore checks use the plain driver, without Store functions.
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	var integrity string
	require.NoError(db.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity))
	assert.Equal(t, "ok", integrity)
}

func TestMeetingLinksUnsupportedMasterException(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, "FREQ=WEEKLY;COUNT=6", "FREQ=HOURLY;COUNT=6"))
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-07T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	from := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270307T100000Z", master), page.Occurrences[0].Ref)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270307T100000Z"}))
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(page.Occurrences[0].Ref, links[0].Ref)
	assertions.Equal(from, links[0].Start)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	links, err = st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	assertions.Empty(links)
	_, otherNote := projectionFixture(t, st, "other-note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: exception}))
	links, err = st.MeetingLinksContext(t.Context(), otherNote)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(page.Occurrences[0].Ref, links[0].Ref)
}

func TestMeetingLinksRejectOrphanExceptionsWithoutCompleteBounds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "cancellation tombstone",
			raw:  `{"id":"orphan-cancelled","recurringEventId":"missing-series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, orphan := calendarFixture(t, st, "orphan", tc.raw)
			_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)

			err := st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan})
			requirements.ErrorIs(err, sql.ErrNoRows)
			assertions.Empty(linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksSuggestionWindowPlan(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "window@example.org"))
	// Enough historical notes make a narrow range cheaper than a population scan.
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"window@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, err := st.db.Exec(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at) SELECT source_id,conversation_id, 'history-' || n, 'meeting_transcript', '2020-01-01 00:00:00+00:00' FROM messages CROSS JOIN `+func() string {
		if st.IsPostgreSQL() {
			return `generate_series(1,3000) AS n`
		}
		return `(WITH RECURSIVE nums(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM nums WHERE n<3000) SELECT n FROM nums)`
	}()+` WHERE id = ?`, note)
	requirements.NoError(err)
	_, err = st.db.Exec(`INSERT INTO meeting_link_key(message_id,kind,value,calendar_id,scheduled_at) SELECT id,'ical_uid','old@example.org','','2020-01-01 00:00:00+00:00' FROM messages WHERE source_message_id LIKE 'history-%'`)
	requirements.NoError(err)
	_, err = st.db.Exec(`ANALYZE`)
	requirements.NoError(err)
	var query string
	var args []any
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, values []driver.NamedValue) error {
		if strings.Contains(q, "WITH window_ids AS") {
			query = q
			for _, v := range values {
				args = append(args, v.Value)
			}
		}
		return nil
	}})
	suggestions, err := observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	requirements.NotEmpty(query)
	explain := "EXPLAIN QUERY PLAN "
	if st.IsPostgreSQL() {
		explain = "EXPLAIN "
	}
	rows, err := st.db.QueryContext(t.Context(), explain+query, args...)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var plan []string
	for rows.Next() {
		var detail string
		if st.IsPostgreSQL() {
			requirements.NoError(rows.Scan(&detail))
		} else {
			var id, parent, unused int
			requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		}
		plan = append(plan, detail)
	}
	requirements.NoError(rows.Err())
	text := strings.Join(plan, "\n")
	assertions.Contains(text, "idx_meeting_link_key_scheduled")
	assertions.Contains(text, "idx_messages_meeting_start")
	if st.IsPostgreSQL() {
		assertions.Contains(text, "Index Cond:")
	}
}

func TestMeetingSuggestionsOffsetCancellation(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	_, err := st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, "2027-03-14T06:00:00-04:00", note)
	require.NoError(err)
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.NoError(err)
	assert.Empty(t, suggestions)
}

func TestMeetingUnsupportedSeriesCandidates(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, "FREQ=WEEKLY;COUNT=6", "FREQ=HOURLY;COUNT=6"))
	calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-07T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.NoError(err)
	if assert.Len(suggestions, 1) {
		assert.Equal(fmt.Sprintf("evt:%d@20270307T100000Z", master), suggestions[0].Ref)
	}
	assert.Equal(fmt.Sprintf("evt:%d@20270307T100000Z", master), linkedMeetingRefs(t, st, note)[note])
	from := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
	require.NoError(err)
	require.Len(page.Unexpandable, 1)
	assert.Equal("unsupported recurrence", page.Unexpandable[0].Reason)
}

func TestMeetingLinkTargetsRequireBounds(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"tombstone", `{"id":"gone","status":"cancelled"}`},
		{"orphan", `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`},
		{"partial orphan", `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", tc.raw)
			_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			err := st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event})
			require.ErrorIs(err, sql.ErrNoRows)
			assertions.Contains(err.Error(), "bounds")
			assertions.Empty(linkedMeetingRefs(t, st, note)[note])
			if tc.name == "tombstone" {
				return
			}
			_, master := calendarFixture(t, st, "master", linkSeriesRaw)
			require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			links, err := st.MeetingLinksContext(t.Context(), note)
			require.NoError(err)
			require.Len(links, 1)
			assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
			assertions.Equal("2027-03-14T10:00:00Z", links[0].Start.Format(time.RFC3339))
			assertions.Equal("2027-03-14T11:00:00Z", links[0].End.Format(time.RFC3339))
		})
	}
}

func TestMeetingDedupRawBackfillProjections(t *testing.T) {
	for _, kind := range []string{"calendar", "note"} {
		t.Run(kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", "dedup@example.org"))
			noteData, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"dedup@example.org"}}`)
			require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			data, duplicate := eventData, event
			if kind == "note" {
				data, duplicate = noteData, note
			}
			data.Message.SourceMessageID = "survivor"
			data.RawMIME = nil
			survivor, err := st.PersistMessage(data)
			require.NoError(err)
			result, err := st.MergeDuplicates(survivor, []int64{duplicate}, "synthetic-merge")
			require.NoError(err)
			assert.Equal(1, result.RawMIMEBackfilled)
			if kind == "calendar" {
				event = survivor
			} else {
				note = survivor
				var uid string
				require.NoError(st.db.QueryRow(`SELECT value FROM meeting_link_key WHERE message_id = ?`, note).Scan(&uid))
				assert.Equal("dedup@example.org", uid)
			}
			assert.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
			from := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
			require.NoError(err)
			require.Len(page.Occurrences, 1)
			assert.Equal(fmt.Sprintf("evt:%d", event), page.Occurrences[0].Ref)
		})
	}
}

func TestMeetingUnsupportedSeriesExceptionWithoutOriginal(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, "FREQ=WEEKLY;COUNT=6", "FREQ=HOURLY;COUNT=6"))
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.NoError(err)
	require.Len(suggestions, 1)
	assert.Equal(fmt.Sprintf("evt:%d", exception), suggestions[0].Ref)
	assert.Equal(fmt.Sprintf("evt:%d", exception), linkedMeetingRefs(t, st, note)[note])
}
