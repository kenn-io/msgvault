package store

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

const linkSeriesRaw = `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`

func TestMeetingLinksCalendarUIDLookupPlan(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "event", singleLinkRaw("event", "metadata@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"metadata@example.org"}}`)
	// Distinct calendar UIDs make an indexed lookup cheaper than scanning events.
	for i := range 3000 {
		key := fmt.Sprintf("history-%d", i)
		calendarFixture(t, st, key, singleLinkRaw(key, key+"@example.org"))
	}
	_, err := st.db.Exec(`ANALYZE messages`)
	requirements.NoError(err)
	_, err = st.db.Exec(`ANALYZE calendar_events`)
	requirements.NoError(err)
	var query string
	var args []any
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, values []driver.NamedValue) error {
		if strings.Contains(q, "FROM calendar_events e") && strings.Contains(q, "ical_uid") && len(values) > 0 && values[0].Value == "metadata@example.org" {
			query = q
			args = nil
			for _, v := range values {
				args = append(args, v.Value)
			}
		}
		return nil
	}})
	assertions.Equal(fmt.Sprintf("evt:%d", id), linkedMeetingRefs(t, observed, note)[note])
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
	planText := strings.Join(plan, "\n")
	assertions.Contains(planText, "idx_messages_calendar_ical_uid")
	if st.IsPostgreSQL() {
		assertions.Regexp(`Index Cond:.*ical_uid`, planText)
	}
}

func TestMeetingLinksWindowedExceptions(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := strings.ReplaceAll(strings.ReplaceAll(linkSeriesRaw, "2027-03-07", "2026-01-04"), "FREQ=WEEKLY;COUNT=6", "FREQ=DAILY;COUNT=1000")
	_, master := calendarFixture(t, st, "master", raw)
	for i := range 201 {
		at := time.Date(2026, 1, 4, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		calendarFixture(t, st, fmt.Sprintf("old-%d", i), fmt.Sprintf(`{"id":"old-%d","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":%q}}`, i, at.Format(time.RFC3339)))
	}
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	requirements.NoError(err)
	assertions.Equal([]int64{note}, record.MessageIDs)

	_, twenty := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, twenty)[twenty])

	// An old exception moved into this window retains its original identity.
	calendarFixture(t, st, "old-1", `{"id":"old-1","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2026-01-05T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	raw = strings.Replace(raw, `"RRULE:FREQ=DAILY;COUNT=1000"`, `"RRULE:FREQ=DAILY;COUNT=1000","EXDATE:20270314T100000Z"`, 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(raw), "gcal_json"))
	assertions.Equal(fmt.Sprintf("evt:%d@20260105T100000Z", master), linkedMeetingRefs(t, st, twenty)[twenty])
}

func TestMeetingLinksTwentyExactInstant(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:30Z"}}`)
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions.Suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), suggestions.Suggestions[0].Ref)
}

func TestMeetingLinksSplitSurvivesReingest(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", "same@example.org"))
	noteData, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	for _, tc := range []struct {
		data *MessagePersistData
		id   int64
	}{{noteData, note}, {eventData, event}} {
		id, err := st.PersistMessage(tc.data)
		requirements.NoError(err)
		assertions.Equal(tc.id, id)
	}
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
}

func singleLinkRaw(id, uid string) string {
	return fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, id, uid)
}
func granolaLinkFixture(t *testing.T, st *Store, event string) int64 {
	t.Helper()
	_, id := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q,"scheduled_start_time":"2027-03-14T10:00:00Z"}}`, event))
	return id
}

func TestMeetingLinksGranolaOwnCalendar(t *testing.T) {
	t.Helper()
	for _, tc := range []struct{ name, calendar, owner string }{
		{"exact email", "calendar@example.com", "calendar@example.com"},
		{"padded uppercase email", " Calendar@Example.COM ", " CALENDAR@example.com "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := singleLinkRaw("event", "own@example.org")
			_, other := calendarFixture(t, st, "other", raw, "other@example.com")
			_, note := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"owner":{"email":%q},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`, tc.owner))
			assertions.Empty(linkedMeetingRefs(t, st, note)[note])
			suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
			requirements.NoError(err)
			requirements.Len(suggestions.Suggestions, 1)
			assertions.Equal(fmt.Sprintf("evt:%d", other), suggestions.Suggestions[0].Ref)
			_, own := calendarFixture(t, st, "own", raw, tc.calendar)
			assertions.Equal(fmt.Sprintf("evt:%d", own), linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksGranolaConflictingCalendarIdentities(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, own := calendarFixture(t, st, "own", singleLinkRaw("event", "own@example.org"))
	_, shared := calendarFixture(t, st, "shared", singleLinkRaw("event", "shared@example.org"), "shared@example.com")
	note := granolaLinkFixture(t, st, "event")
	assert.Empty(t, linkedMeetingRefs(t, st, note)[note])
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.NoError(t, err)
	var refs []string
	for _, suggestion := range page.Suggestions {
		refs = append(refs, suggestion.Ref)
	}
	assert.ElementsMatch(t, []string{fmt.Sprintf("evt:%d", own), fmt.Sprintf("evt:%d", shared)}, refs)
}

func TestMeetingGranolaSingleEventLinkSurvivesReschedule(t *testing.T) {
	for _, scheduled := range []string{`,"scheduled_start_time":"2027-03-14T10:00:00Z"`, ""} {
		t.Run("scheduled="+strconv.FormatBool(scheduled != ""), func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := singleLinkRaw("event", "event@example.org")
			_, event := calendarFixture(t, st, "event", raw)
			_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event"`+scheduled+`}}`)
			want := fmt.Sprintf("evt:%d", event)
			require.Equal(want, linkedMeetingRefs(t, st, note)[note])
			calendarFixture(t, st, "event", strings.ReplaceAll(raw, "2027-03-14", "2027-03-15"))
			links, err := meetingLinksForTest(t, st, note)
			require.NoError(err)
			require.Len(links, 1)
			assert.Equal(want, links[0].Ref)
			assert.Equal("2027-03-15T10:00:00Z", links[0].Start.UTC().Format(time.RFC3339))
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
			require.NoError(err)
			assert.Equal([]int64{note}, record.MessageIDs)
		})
	}
}

func TestMeetingLinksIdentityHasSeparateFields(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, single := calendarFixture(t, st, "single", singleLinkRaw("single", "x@20270314T100000Z"))
	_, series := calendarFixture(t, st, "series", strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", "x"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"x@20270314T100000Z","startsAt":"2027-03-14T10:00:00Z"}}`)
	assertions.Equal(fmt.Sprintf("evt:%d", single), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: series, Key: "20270314T100000Z"}))
	assertions.Equal(fmt.Sprintf("evt:%d", single), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: single}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingLinksGranolaOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name, raw, exception, key, scheduled string
		want                                 bool
	}{
		{name: "generated", raw: linkSeriesRaw, want: true},
		{name: "exdate", raw: strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1)},
		{name: "cancelled override", raw: strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1), exception: `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`, want: true},
		{name: "moved", raw: linkSeriesRaw, exception: `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-04-01T12:00:00Z"},"end":{"dateTime":"2027-04-01T13:00:00Z"}}`, want: true},
		{name: "non UTC", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T05:00:00-05:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-07T06:00:00-05:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`},
		{name: "non UTC after DST", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T06:00:00-05:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-07T07:00:00-05:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, want: true},
		{name: "all day", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`},
		{name: "all day instance", raw: `{"id":"series","iCalUID":"day@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, key: "20270314", scheduled: "2027-03-14T00:00:00Z", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", tc.raw)
			if tc.exception != "" {
				calendarFixture(t, st, "exception", tc.exception)
			}
			key := tc.key
			if key == "" {
				key = "20270314T100000Z"
			}
			_, note := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q,"scheduled_start_time":%q}}`, "series_"+key, cmp.Or(tc.scheduled, "2027-03-14T10:00:00Z")))
			want := ""
			if tc.want {
				want = fmt.Sprintf("evt:%d@%s", master, key)
			}
			assert.Equal(t, want, linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksTwentyCopiesAndAmbiguity(t *testing.T) {
	t.Run("moved copy", testMeetingUIDMovedSingleCopy)
	t.Run("copy precedence fallback", testMeetingCopyPrecedenceFallback)
	for _, kind := range []string{"single copies", "series copies", "different occurrences"} {
		t.Run(kind, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			raw := linkSeriesRaw
			key := "@20270314T100000Z"
			if kind == "single copies" {
				raw = singleLinkRaw("one", "weekly@example.org")
				key = ""
			}
			_, one := calendarFixture(t, st, "one", raw)
			calendarFixture(t, st, "copy", raw, "copy@example.com")
			_, note := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			want := fmt.Sprintf("evt:%d%s", one, key)
			if kind == "different occurrences" {
				calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
				want = ""
			}
			assert.Equal(t, want, linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksUserDecisions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	raw := singleLinkRaw("event", "same@example.org")
	_, event := calendarFixture(t, st, "one", raw)
	_, copyID := calendarFixture(t, st, "copy", raw, "copy@example.com")
	_, other := calendarFixture(t, st, "other", singleLinkRaw("other", "other@example.org"))
	note := granolaLinkFixture(t, st, "event")
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: other}))
	assertions.Equal(fmt.Sprintf("evt:%d", other), linkedMeetingRefs(t, st, note)[note])
	requirements.ErrorIs(st.LinkMeetingContext(t.Context(), event, OccurrenceRef{MessageID: other}), sql.ErrNoRows)
	requirements.ErrorIs(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: 999999}), sql.ErrNoRows)
}

func TestMeetingLinksOrphanDecisionAfterMasterArrives(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`)
	requirements.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	assertions.Equal(fmt.Sprintf("evt:%d,evt:%d", automatic, orphan), linkedMeetingRefs(t, st, note)[note])
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	assertions.Equal(fmt.Sprintf("evt:%d,evt:%d@20270314T100000Z", automatic, master), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note])
	calendarFixture(t, st, "master", strings.Replace(linkSeriesRaw, `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]`, "", 1))
	assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "removing recurrence must preserve the owner's split and restore automatic matches")
}

func TestMeetingLinksActualRecurrenceBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"series","iCalUID":"gaps@example.org","start":{"dateTime":"2027-03-07T02:30:00-08:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-03-07T03:30:00-08:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=260"]}`)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	r := st.meetingReader()
	budget := calrecur.RequestReadBudget
	ref := OccurrenceRef{MessageID: master, Key: "20320404T093000Z"}
	o, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.NoError(err)
	assertions.Equal("20320404T093000Z", o.Ref.Key)
	spent := calrecur.RequestReadBudget - budget
	assertions.Greater(spent, 260)
	budget = spent
	_, err = resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.NoError(err)
	assertions.Zero(budget)
	budget = spent - 1
	_, err = resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.ErrorIs(err, ErrQueryTooBroad)
}

func TestMeetingLinksProjectionEnvelopeAndRepair(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "valid@example.org"))
	data, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":2,"recording":{},"calendar_event":{"iCalUid":"valid@example.org"}}`)
	requirements.NoError(st.SetMessageMetadata(note, sql.NullString{String: `{"platform":"twenty","calendar_ical_uid":"valid@example.org"}`, Valid: true}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	valid := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"valid@example.org"}}`
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(valid), "twenty_json"))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	// Only the shared ID changes; the content remains identical.
	before, hash := readProjection(t, st, note)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(strings.ReplaceAll(valid, "valid@example.org", "changed@example.org")), "twenty_json"))
	after, newHash := readProjection(t, st, note)
	assertions.Equal(before, after)
	assertions.NotEqual(hash, newHash)
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(`{broken`), "twenty_json"))
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_key WHERE message_id = ?`, note).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(valid), "twenty_json"))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	data.Message.MessageType = "email"
	_, err := st.UpsertMessage(data.Message)
	requirements.NoError(err)
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_key WHERE message_id = ?`, note).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingLinksUpgradeFromSchema31(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "upgraded@example.org"))
	note := granolaLinkFixture(t, st, "event")
	_, err := st.db.Exec(`DROP TABLE meeting_link_decisions; DROP TABLE meeting_link_key; DROP INDEX idx_messages_calendar_ical_uid`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_details SET projection_version = 1; UPDATE archive_metadata SET value = '31' WHERE key = 'schema_version'`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE applied_migrations SET version = 1 WHERE name = ?`, migrationMeetingProjectionV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingLinksReadDecisions(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	note := granolaLinkFixture(t, st, "event")
	links, err := meetingLinksForTest(t, st, note)
	requirements.NoError(err)
	assertions.Empty(links)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "linked@example.org"))
	links, err = meetingLinksForTest(t, st, note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), links[0].Ref)
	assertions.Equal("Example event", links[0].Subject)
	assertions.Equal("2027-03-14T10:00:00Z", links[0].Start.Format(time.RFC3339))
	assertions.Equal("2027-03-14T11:00:00Z", links[0].End.Format(time.RFC3339))
	_, err = meetingLinksForTest(t, st, event)
	assertions.ErrorIs(err, sql.ErrNoRows)
}

// Read the same references returned to CLI and API users.
func linkedMeetingRefs(t *testing.T, st *Store, ids ...int64) map[int64]string {
	t.Helper()
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		links, err := meetingLinksForTest(t, st, id)
		require.NoError(t, err)
		var refs []string
		for _, link := range links {
			refs = append(refs, link.Ref)
		}
		out[id] = strings.Join(refs, ",")
	}
	return out
}

func TestMeetingLinksPrimaryCalendar(t *testing.T) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "event", singleLinkRaw("event", "primary@example.org"))
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"primary","account_email":"Calendar@Example.COM","primary":true,"time_zone":"UTC"}`))
	note := granolaLinkFixture(t, st, "event")
	links, err := meetingLinksForTest(t, st, note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), links[0].Ref)
}

func TestMeetingLinksTwoOccurrenceAliases(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	links, err := meetingLinksForTest(t, st, note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
	assertions.Equal("2027-03-14T12:00:00Z", links[0].Start.Format(time.RFC3339))
}

func TestMeetingLinksUnidentifiedExceptionsUseMessageIdentity(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "orphan-one", `{"id":"orphan-one","iCalUID":"shared@example.org","recurringEventId":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, second := calendarFixture(t, st, "orphan-two", `{"id":"orphan-two","iCalUID":"shared@example.org","recurringEventId":"series","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)

	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: first}))
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: second}))
	links, err := meetingLinksForTest(t, st, note)
	requirements.NoError(err)
	requirements.Len(links, 2)
	assertions.ElementsMatch([]string{fmt.Sprintf("evt:%d", first), fmt.Sprintf("evt:%d", second)}, []string{links[0].Ref, links[1].Ref})

	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: first}))
	links, err = meetingLinksForTest(t, st, note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", second), links[0].Ref)
}

func TestMeetingDecisionSourceRemoval(t *testing.T) {
	for _, uid := range []string{"lifetime@example.org", ""} {
		t.Run("uid="+uid, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			noteData, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			requirements.NoError(st.RemoveSource(data.Message.SourceID))
			links, err := meetingLinksForTest(t, st, note)
			requirements.NoError(err)
			assertions.Empty(links)
			var count int
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
			assertions.Zero(count, "decisions follow the target message's lifetime")
			calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			links, err = meetingLinksForTest(t, st, note)
			requirements.NoError(err)
			assertions.Empty(links)
			requirements.NoError(st.RemoveSource(noteData.Message.SourceID))
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions`).Scan(&count))
			assertions.Zero(count, "decisions follow the member note's lifetime")
		})
	}
}

func meetingLinksForTest(t *testing.T, st *Store, id int64) ([]MeetingLink, error) {
	t.Helper()
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
	return record.Links, err
}

func testMeetingUIDMovedSingleCopy(t *testing.T) {
	t.Helper()
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := singleLinkRaw("old", "moved@example.org")
	calendarFixture(t, st, "old", raw)
	_, moved := calendarFixture(t, st, "moved", strings.NewReplacer("old", "moved", "10:00:00", "12:00:00", "11:00:00", "13:00:00").Replace(raw))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"moved@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: moved}), links[0].Ref)
	assert.Equal(time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC), links[0].Start)
}

func testMeetingCopyPrecedenceFallback(t *testing.T) {
	t.Helper()
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "first", singleLinkRaw("first", "copies@example.org"))
	calendarFixture(t, st, "second", singleLinkRaw("second", "copies@example.org"), "second@example.com")
	for _, scheduled := range []string{"", "2027-03-14T15:00:00Z"} {
		start := "null"
		if scheduled != "" {
			start = fmt.Sprintf("%q", scheduled)
		}
		_, note := projectionFixture(t, st, "note"+scheduled, "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"copies@example.org","startsAt":`+start+`}}`)
		links, err := meetingLinksForTest(t, st, note)
		require.NoError(err)
		require.Len(links, 1)
		assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: first}), links[0].Ref)
	}
}
