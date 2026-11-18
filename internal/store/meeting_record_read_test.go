package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestMeetingRecordSourceScope(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	eventData, event := calendarFixture(t, st, "event", `{"id":"event","iCalUID":"meeting@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "calendar@example.com")
	noteData, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, indirect := projectionFixture(t, st, "indirect", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event"}}`)
	for _, tc := range []struct {
		name        string
		sources     []int64
		ref         MeetingLinkRef
		wantNotes   []int64
		wantEvent   bool
		unavailable bool
	}{
		{"event with both sources", []int64{noteData.Message.SourceID, eventData.Message.SourceID}, MeetingLinkRef{Kind: "evt", MessageID: event}, []int64{note, indirect}, true, false},
		{"note with notes source", []int64{noteData.Message.SourceID}, MeetingLinkRef{Kind: "msg", MessageID: note}, []int64{note}, false, false},
		{"note outside scope", []int64{eventData.Message.SourceID}, MeetingLinkRef{Kind: "msg", MessageID: note}, nil, false, true},
		{"event with calendar source", []int64{eventData.Message.SourceID}, MeetingLinkRef{Kind: "evt", MessageID: event}, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := sourcescope.WithSourceIDs(t.Context(), tc.sources)
			record, err := st.MeetingRecordContext(ctx, tc.ref)
			if tc.unavailable {
				require.ErrorIs(err, sql.ErrNoRows)
				return
			}
			require.NoError(err)
			assert.ElementsMatch(tc.wantNotes, record.MessageIDs)
			if tc.wantEvent {
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
			} else {
				assert.Empty(record.Links)
			}
		})
	}
}

func TestMeetingRecordLinkedTranscriptsWithinSourceScope(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, first := projectionFixture(t, st, "first", "meeting_json", `{}`)
	_, second := projectionFixture(t, st, "second", "meeting_json", `{}`)
	_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: first}, MeetingLinkRef{Kind: "msg", MessageID: second}, "link")
	require.NoError(err)
	other, err := st.GetOrCreateSource("meeting_import", "other@example.test")
	require.NoError(err)
	for _, sources := range [][]int64{{data.Message.SourceID}, {data.Message.SourceID, other.ID}} {
		ctx := sourcescope.WithSourceIDs(t.Context(), sources)
		record, err := st.MeetingRecordContext(ctx, MeetingLinkRef{Kind: "msg", MessageID: first})
		require.NoError(err)
		assert.ElementsMatch([]int64{first, second}, record.MessageIDs)
	}
}

func TestMeetingRecordUntimedCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, raw, status, reason string
	}{
		{"cancelled", `{"id":"event_20270314T100000Z","iCalUID":"uid@example.org","status":"cancelled"}`, "cancelled", "missing event bounds"},
		{"decode error", `{"id":`, "decode_error", "event data could not be decoded"},
		{"exception", `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled"}`, "cancelled", "exception has no original start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, tc.name, tc.raw, "owner@example.com")
			seeds := []MeetingLinkRef{{Kind: "evt", MessageID: event}}
			var note, uidNote int64
			switch tc.name {
			case "cancelled":
				_, err := st.db.Exec(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`, time.Now(), event)
				require.NoError(err)
				_, note = projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z"}}`)
				_, uidNote = projectionFixture(t, st, "uid-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			case "exception":
				_, note = projectionFixture(t, st, "own", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series_20270314T100000Z"}}`)
			}
			if note != 0 {
				seeds = append(seeds, MeetingLinkRef{Kind: "msg", MessageID: note})
			}
			for _, seed := range seeds {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
				assert.Equal(event, record.Links[0].MessageID)
				assert.Equal(tc.status, record.Links[0].Status)
				assert.Equal("Example event", record.Links[0].Subject)
				assert.Nil(record.Links[0].Start)
				assert.Nil(record.Links[0].End)
				if note == 0 {
					assert.Empty(record.MessageIDs)
				} else {
					assert.Equal([]int64{note}, record.MessageIDs)
				}
				if tc.name == "cancelled" {
					assert.Zero(record.Links[0].SubjectOriginID)
					assert.True(record.Links[0].DeletedFromSource)
					assert.NotContains(record.MessageIDs, uidNote)
				}
				assert.Equal([]CalendarUnexpandable{{MessageID: event, Reason: tc.reason}}, record.Unexpandable)
			}
		})
	}
}

func TestMeetingRecordStandaloneCancellationPartialBounds(t *testing.T) {
	for _, tc := range []struct {
		name, field, bound, noteStart, recurrence string
		want                                      time.Time
	}{
		{"start timed", "start", `{"dateTime":"2027-03-14T10:00:00Z"}`, "2027-03-14T10:00:00Z", "", time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)},
		{"start all-day", "start", `{"date":"2027-03-14","timeZone":"UTC"}`, "2027-03-14T00:00:00Z", "", time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)},
		{"end timed", "end", `{"dateTime":"2027-03-14T11:00:00Z"}`, "", "", time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC)},
		{"end all-day", "end", `{"date":"2027-03-15","timeZone":"UTC"}`, "", "", time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)},
		{"supported-start", "start", `{"dateTime":"2027-03-15T10:00:00Z"}`, "", "RRULE:FREQ=WEEKLY;COUNT=2", time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC)},
		{"supported-end", "end", `{"dateTime":"2027-03-15T11:00:00Z"}`, "", "RRULE:FREQ=WEEKLY;COUNT=2", time.Date(2027, 3, 15, 11, 0, 0, 0, time.UTC)},
		{"unsupported-start", "start", `{"dateTime":"2027-03-15T10:00:00Z"}`, "", "RRULE:FREQ=YEARLY;BYWEEKNO=20", time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC)},
		{"unsupported-end", "end", `{"dateTime":"2027-03-15T11:00:00Z"}`, "", "RRULE:FREQ=YEARLY;BYWEEKNO=20", time.Date(2027, 3, 15, 11, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			extra, reason := "", "missing event bounds"
			if tc.recurrence != "" {
				calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":[%q]}`, tc.recurrence), "owner@example.com")
				extra, reason = `,"recurringEventId":"series"`, "exception has no original start"
			} else if tc.field == "start" {
				extra = `,"iCalUID":"standalone@example.org"`
			}
			_, event := calendarFixture(t, st, "cancelled", fmt.Sprintf(`{"id":"cancelled","status":"cancelled",%q:%s%s}`, tc.field, tc.bound, extra), "owner@example.com")
			var note int64
			if tc.noteStart != "" {
				_, note = projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"standalone@example.org","startsAt":%q}}`, tc.noteStart))
			} else {
				_, note = projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"cancelled"}}`)
			}
			for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "msg", MessageID: note}} {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
				if tc.field == "start" {
					require.NotNil(record.Links[0].Start)
					assert.Equal(tc.want, *record.Links[0].Start)
					assert.Nil(record.Links[0].End)
				} else {
					assert.Nil(record.Links[0].Start)
					require.NotNil(record.Links[0].End)
					assert.Equal(tc.want, *record.Links[0].End)
				}
				assert.Equal("cancelled", record.Links[0].Status)
				assert.Equal([]int64{note}, record.MessageIDs)
				warning := CalendarUnexpandable{MessageID: event, Reason: reason}
				if tc.recurrence == "" {
					assert.Equal([]CalendarUnexpandable{warning}, record.Unexpandable)
				} else {
					assert.Contains(record.Unexpandable, warning)
				}
			}
		})
	}
}

func TestMeetingRecordCancelledOccurrenceSparseMaster(t *testing.T) {
	t.Run("before master", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, event := calendarFixture(t, st, "sparse", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`, "owner@example.com")
		_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series_20270314T100000Z"}}`)
		for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: event}} {
			record, err := st.MeetingRecordContext(t.Context(), seed)
			require.NoError(err)
			require.Len(record.Links, 1)
			assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
			assert.Equal("cancelled", record.Links[0].Status)
			assert.Equal([]int64{note}, record.MessageIDs)
			assert.Equal([]CalendarUnexpandable{{MessageID: event, Reason: "missing series master and event bounds"}}, record.Unexpandable)
			encoded, err := json.Marshal(record.Links[0])
			require.NoError(err)
			assert.Contains(string(encoded), `"start":null`)
			assert.Contains(string(encoded), `"end":null`)
		}
	})
	for _, tc := range []struct {
		name, original, eventID, start, end, identity, recurrence string
		supported, typeChange                                     bool
	}{
		{"timed unsupported", `{"dateTime":"2027-03-14T10:00:00Z"}`, "series_20270314T100000Z", `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-14T11:00:00Z"}`, "20270314T100000Z", "RRULE:FREQ=YEARLY;BYWEEKNO=20", false, false},
		{"all-day unsupported", `{"date":"2027-03-14"}`, "series_20270314", `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, "20270314", "RRULE:FREQ=YEARLY;BYWEEKNO=20", false, false},
		{"all-day supported", `{"date":"2027-03-14"}`, "series_20270314", `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, "20270314", "RRULE:FREQ=WEEKLY;COUNT=2", true, false},
		{"all-day to timed", `{"date":"2027-03-09"}`, "cancelled", `{"dateTime":"2027-03-02T09:00:00Z"}`, `{"dateTime":"2027-03-02T10:00:00Z"}`, "20270309", "RRULE:FREQ=WEEKLY;COUNT=3", false, true},
		{"timed to all-day", `{"dateTime":"2027-03-09T09:00:00Z"}`, "cancelled", `{"date":"2027-03-02"}`, `{"date":"2027-03-03"}`, "20270309T090000Z", "RRULE:FREQ=WEEKLY;COUNT=3", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "sparse", fmt.Sprintf(`{"id":%q,"recurringEventId":"series","originalStartTime":%s,"status":"cancelled"}`, tc.eventID, tc.original), "owner@example.com")
			_, note := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":%q}}`, tc.eventID))
			_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"series","start":%s,"end":%s,"recurrence":[%q]}`, tc.start, tc.end, tc.recurrence), "owner@example.com")
			seeds := []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, tc.identity)}}
			if !tc.supported {
				seeds = append(seeds, MeetingLinkRef{Kind: "msg", MessageID: note})
			}
			if tc.typeChange {
				seeds = []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "msg", MessageID: note}}
			}
			for _, seed := range seeds {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d@%s", master, tc.identity), record.Links[0].Ref)
				if tc.supported {
					assert.NotNil(record.Links[0].Start)
					assert.NotNil(record.Links[0].End)
					assert.Empty(record.Unexpandable)
				} else {
					assert.Nil(record.Links[0].Start)
					assert.Nil(record.Links[0].End)
					assert.Equal([]int64{note}, record.MessageIDs)
					if !tc.typeChange {
						assert.Contains(record.Unexpandable, CalendarUnexpandable{MessageID: master, Reason: "unsupported recurrence"})
					}
				}
			}
		})
	}
}

func TestMeetingRecordZeroStartNoteNotLinked(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, sparse := calendarFixture(t, st, "no-start", `{"id":"no-start","recurringEventId":"absent","status":"cancelled","iCalUID":"zero@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`, "owner@example.com")
	_, zero := projectionFixture(t, st, "zero-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zero@example.org","startsAt":"0001-01-01T00:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: sparse})
	require.NoError(err)
	assert.NotContains(record.MessageIDs, zero)
}

func TestMeetingRecordOrphanAllDayDoesNotGuessOriginalZone(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "orphan", `{"id":"edit","recurringEventId":"series","iCalUID":"zone@example.org","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15","timeZone":"Asia/Tokyo"},"end":{"date":"2027-03-16","timeZone":"Asia/Tokyo"}}`, "owner@example.com")
	_, unrelated := projectionFixture(t, st, "unrelated", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zone@example.org","startsAt":"2027-03-13T15:00:00Z"}}`)
	_, matched := projectionFixture(t, st, "matching", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"edit"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
	require.NoError(err)
	require.Len(record.Links, 1)
	assert.Contains(record.MessageIDs, matched)
	assert.NotContains(record.MessageIDs, unrelated)
}

func TestMeetingRecordNoteLookupSharesBudget(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z"}}`
	projectionFixture(t, st, "first", "granola_json", raw)
	projectionFixture(t, st, "second", "granola_json", raw)
	budget := 1
	_, err := st.meetingNoteKeys(t.Context(), []string{canonicalMeetingKey("google_event_id", "owner@example.com", "event_20270314T100000Z", nil).text()}, &budget)
	assert.ErrorIs(t, err, ErrQueryTooBroad)
}

func TestMeetingRecordReportsUnsupportedSeries(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "unsupported-series", `{"id":"unsupported","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
	_, supportedCopy := calendarFixture(t, st, "supported-copy", `{"id":"copy","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	for _, ref := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270314T100000Z")}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Equal([]CalendarUnexpandable{{MessageID: series, Reason: "unsupported recurrence"}}, record.Unexpandable)
		if ref.Kind == "msg" {
			requirements.Len(record.Links, 1)
			assertions.Equal(fmt.Sprintf("evt:%d", supportedCopy), record.Links[0].Ref)
			assertions.Equal([]int64{note}, record.MessageIDs)
		} else {
			assertions.NotNil(record.Links)
			assertions.NotNil(record.MessageIDs)
			assertions.NotNil(record.Unexpandable)
			assertions.Empty(record.Links)
			assertions.Empty(record.MessageIDs)
		}
	}
	_, supported := calendarFixture(t, st, "supported-series", `{"id":"supported","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: supported, Identity: occurrenceIdentity(t, "20270314T100000Z")})
	requirements.NoError(err)
	assertions.NotNil(record.Unexpandable)
	assertions.Empty(record.Unexpandable)

	t.Run("later-date", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, series := calendarFixture(t, st, "unsupported-series", `{"id":"unsupported","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
		_, note := projectionFixture(t, st, "unproven-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
		calendarFixture(t, st, "unproven-copy", `{"id":"copy","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-21T10:00:00Z"},"end":{"dateTime":"2027-03-21T11:00:00Z"}}`)
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270321T100000Z")})
		require.NoError(err)
		assert.Equal([]CalendarUnexpandable{{MessageID: series, Reason: "unsupported recurrence"}}, record.Unexpandable)
		assert.NotContains(record.MessageIDs, note)
		assert.Empty(record.Links)
	})
}

func TestMeetingRecordReadsNotesBeforeCalendarImport(t *testing.T) {
	for _, tc := range []struct{ name, format, first, second, other string }{
		{"twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, ""},
		{"granola", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z","scheduled_start_time":"2027-03-14T10:00:00Z"}}`, `{"owner":{"email":"OWNER@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z","scheduled_start_time":"2027-03-14T12:00:00Z"}}`, `{"owner":{"email":"other@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, first := projectionFixture(t, st, "first", tc.format, tc.first)
			_, second := projectionFixture(t, st, "second", tc.format, tc.second)
			for _, id := range []int64{first, second} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
				require.NoError(err)
				assert.Equal([]int64{first, second}, record.MessageIDs)
			}
			if tc.other != "" {
				_, other := projectionFixture(t, st, "other", tc.format, tc.other)
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: other})
				require.NoError(err)
				assert.Equal([]int64{other}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingRecordMovedOccurrenceCollidesWithNextWeek(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master-review", `{"id":"review-series","iCalUID":"review@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, moved := calendarFixture(t, st, "moved-review", `{"id":"review-moved","recurringEventId":"review-series","iCalUID":"review@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-21T10:00:00Z"},"end":{"dateTime":"2027-03-21T11:00:00Z"}}`)
	_, first := projectionFixture(t, st, "first-review", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"review@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "second-review", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"review@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	for _, tc := range []struct {
		name  string
		ref   MeetingLinkRef
		want  []int64
		links int
		link  int64
	}{
		{"master original", MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270314T100000Z")}, []int64{first}, 1, moved},
		{"moved", MeetingLinkRef{Kind: "evt", MessageID: moved}, []int64{first}, 1, moved},
		{"original note", MeetingLinkRef{Kind: "msg", MessageID: first}, []int64{first}, 1, moved},
		{"next week", MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270321T100000Z")}, []int64{}, 1, master},
		{"ambiguous note", MeetingLinkRef{Kind: "msg", MessageID: second}, []int64{second}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			record, err := st.MeetingRecordContext(t.Context(), tc.ref)
			require.NoError(err)
			require.Equal(tc.want, record.MessageIDs)
			require.Len(record.Links, tc.links)
			if tc.links > 0 {
				require.Equal(tc.link, record.Links[0].MessageID)
				if tc.link == moved {
					require.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), record.Links[0].Ref)
					require.Equal(time.Date(2027, 3, 21, 10, 0, 0, 0, time.UTC), *record.Links[0].Start)
				}
			}
		})
	}
	_, single := calendarFixture(t, st, "single-review", `{"id":"single","iCalUID":"review@example.org","start":{"dateTime":"2027-03-21T10:00:00Z"},"end":{"dateTime":"2027-03-21T11:00:00Z"}}`)
	_, peer := projectionFixture(t, st, "peer-review", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"review@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	for _, id := range []int64{second, peer} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
		require.NoError(t, err)
		assert.Empty(t, record.Links)
		assert.Equal(t, []int64{second, peer}, record.MessageIDs)
	}
	for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: moved}, {Kind: "evt", MessageID: single}, {Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270321T100000Z")}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		assert.NotContains(t, record.MessageIDs, second)
		assert.NotContains(t, record.MessageIDs, peer)
	}
}

func TestMeetingRecordCancelledAndOrphanOccurrence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled"}`)
	_, cancelledNote := projectionFixture(t, st, "cancelled-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: cancelled}, {Kind: "msg", MessageID: cancelledNote}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d@20270321T100000Z", master), record.Links[0].Ref)
		assert.Equal([]int64{cancelledNote}, record.MessageIDs)
	}
	_, orphan := calendarFixture(t, st, "orphan-moved", `{"id":"orphan","recurringEventId":"absent","iCalUID":"orphan@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`)
	_, orphanNote := projectionFixture(t, st, "orphan-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"orphan@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: orphan}, {Kind: "msg", MessageID: orphanNote}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d", orphan), record.Links[0].Ref)
		assert.Equal([]int64{orphanNote}, record.MessageIDs)
	}
}

func TestMeetingRecordAllDayOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name, raw    string
		keys, starts []string
	}{
		{"timed", `{"id":"timed","iCalUID":"timed@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, []string{"20270314T100000Z", "20270321T100000Z"}, []string{"2027-03-14T10:00:00Z", "2027-03-21T10:00:00Z"}},
		{"all-day", `{"id":"all-day","iCalUID":"all-day@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, []string{"20270314", "20270321"}, []string{"2027-03-14T00:00:00Z", "2027-03-21T00:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, tc.name, tc.raw)
			var notes []int64
			for i, start := range tc.starts {
				_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"%s@example.org","startsAt":%q}}`, tc.name, start))
				notes = append(notes, note)
			}
			for i, key := range tc.keys {
				seeds := []MeetingLinkRef{{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, key)}}
				if tc.name == "all-day" && i == 0 {
					seeds = append(seeds, MeetingLinkRef{Kind: "msg", MessageID: notes[i]})
				}
				for _, seed := range seeds {
					record, err := st.MeetingRecordContext(t.Context(), seed)
					require.NoError(err)
					require.Len(record.Links, 1)
					assert.Equal(fmt.Sprintf("evt:%d@%s", master, key), record.Links[0].Ref)
					require.NotNil(record.Links[0].Start)
					assert.Equal(tc.starts[i], record.Links[0].Start.UTC().Format(time.RFC3339))
					assert.Equal([]int64{notes[i]}, record.MessageIDs)
				}
			}
		})
	}
}

func TestMeetingRecordAllDayEditOriginalZone(t *testing.T) {
	for _, tc := range []struct {
		name, recurrence, original, movedStart, movedEnd, noteStart, identity, expectedStart string
		supported, timed, unavailable                                                        bool
	}{
		{"supported", "RRULE:FREQ=WEEKLY;COUNT=2", "2027-03-21", "2027-03-22", "2027-03-23", "2027-03-21T04:00:00Z", "20270321", "", true, false, false},
		{"unsupported", "RRULE:FREQ=YEARLY;BYWEEKNO=20", "2027-03-14", "2027-03-15", "2027-03-16", "2027-03-14T05:00:00Z", "20270314", "2027-03-14T15:00:00Z", false, false, false},
		{"timed unsupported", "RRULE:FREQ=YEARLY;BYWEEKNO=20", "2027-03-14T10:00:00Z", "2027-03-15T12:00:00Z", "2027-03-15T13:00:00Z", "2027-03-14T10:00:00Z", "20270314T100000Z", "2027-03-15T12:00:00Z", false, true, false},
		{"unavailable zone", "RRULE:FREQ=WEEKLY;COUNT=2", "2027-03-14", "2027-03-15T10:00:00Z", "2027-03-15T11:00:00Z", "2027-03-14T00:00:00Z", "20270314", "2027-03-15T10:00:00Z", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			masterRaw := fmt.Sprintf(`{"id":"series","iCalUID":"zone@example.org","start":{"date":"2027-03-14","timeZone":"America/New_York"},"end":{"date":"2027-03-15","timeZone":"America/New_York"},"recurrence":[%q]}`, tc.recurrence)
			editRaw := fmt.Sprintf(`{"id":"edit","recurringEventId":"series","iCalUID":"zone@example.org","originalStartTime":{"date":%q},"start":{"date":%q,"timeZone":"Asia/Tokyo"},"end":{"date":%q,"timeZone":"Asia/Tokyo"}}`, tc.original, tc.movedStart, tc.movedEnd)
			if tc.timed {
				masterRaw = fmt.Sprintf(`{"id":"series","iCalUID":"zone@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":[%q]}`, tc.recurrence)
				editRaw = fmt.Sprintf(`{"id":"edit","recurringEventId":"series","iCalUID":"zone@example.org","originalStartTime":{"dateTime":%q},"start":{"dateTime":%q},"end":{"dateTime":%q}}`, tc.original, tc.movedStart, tc.movedEnd)
			}
			if tc.unavailable {
				masterRaw = `{"id":"series","iCalUID":"zone@example.org","start":{"dateTime":"2027-03-14T10:00:00Z","timeZone":"Unavailable/Zone"},"end":{"dateTime":"2027-03-14T11:00:00Z","timeZone":"Unavailable/Zone"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`
				editRaw = fmt.Sprintf(`{"id":"edit","recurringEventId":"series","iCalUID":"zone@example.org","originalStartTime":{"date":%q},"start":{"dateTime":%q,"timeZone":"UTC"},"end":{"dateTime":%q,"timeZone":"UTC"}}`, tc.original, tc.movedStart, tc.movedEnd)
			}
			data, master := calendarFixture(t, st, "master", masterRaw, "owner@example.com")
			if !tc.timed && !tc.unavailable {
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","time_zone":"America/New_York"}`))
			}
			_, edit := calendarFixture(t, st, "edit", editRaw, "owner@example.com")
			_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zone@example.org","startsAt":%q}}`, tc.noteStart))
			seeds := []MeetingLinkRef{{Kind: "evt", MessageID: edit}, {Kind: "msg", MessageID: note}}
			var midnight int64
			if tc.unavailable {
				midnight = note
				_, note = projectionFixture(t, st, "exact", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"edit"}}`)
				seeds = []MeetingLinkRef{{Kind: "evt", MessageID: edit}}
				window := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 0, 1))
				require.NoError(err)
				require.Len(page.Occurrences, 1)
				assert.Equal(edit, page.Occurrences[0].MessageID)
				assert.Equal(time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC), page.Occurrences[0].Start)
				assert.Equal(time.Date(2027, 3, 15, 11, 0, 0, 0, time.UTC), page.Occurrences[0].End)
				assert.Contains(page.Unexpandable, CalendarUnexpandable{MessageID: master, Reason: "unsupported recurrence"})
			}
			if !tc.supported && !tc.unavailable {
				seeds = append(seeds, MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, tc.identity)})
			}
			for _, seed := range seeds {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				if tc.supported {
					assert.Equal(fmt.Sprintf("evt:%d@%s", master, tc.identity), record.Links[0].Ref)
				} else {
					assert.Equal(fmt.Sprintf("evt:%d@%s", master, tc.identity), record.Links[0].Ref)
					require.NotNil(record.Links[0].Start)
					assert.Equal(tc.expectedStart, record.Links[0].Start.UTC().Format(time.RFC3339))
					assert.Equal([]CalendarUnexpandable{{MessageID: master, Reason: "unsupported recurrence"}}, record.Unexpandable)
				}
				if tc.unavailable {
					require.NotNil(record.Links[0].End)
					assert.Equal(time.Date(2027, 3, 15, 11, 0, 0, 0, time.UTC), *record.Links[0].End)
					assert.NotContains(record.MessageIDs, midnight)
				}
				assert.Equal([]int64{note}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingRecordCalendarStateAndMissing(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for _, tc := range []struct {
		name, status string
		deleted      bool
	}{
		{"confirmed", "confirmed", false},
		{"cancelled", "cancelled", false},
		{"deleted", "confirmed", true},
	} {
		_, id := calendarFixture(t, st, tc.name, fmt.Sprintf(`{"id":%q,"status":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, tc.name, tc.status))
		if tc.deleted {
			_, err := st.db.Exec(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`, time.Now(), id)
			require.NoError(err)
		}
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(tc.status, record.Links[0].Status)
		assert.Equal(tc.deleted, record.Links[0].DeletedFromSource)
		assert.Empty(record.Unexpandable)
	}
	_, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: 999999})
	assert.ErrorIs(err, sql.ErrNoRows)
}

// Granola documents this occurrence ID in https://docs.granola.ai/api-reference/get-note.
func TestMeetingRecordScopedGoogleIDs(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, own := calendarFixture(t, st, "own", `{"id":"2su99n6iiik37iiknmb5t4fkfh_20260127T153000Z","iCalUID":"own@example.org","start":{"dateTime":"2026-01-27T15:30:00Z"},"end":{"dateTime":"2026-01-27T16:30:00Z"}}`, "owner@example.com")
	_, shared := calendarFixture(t, st, "shared", `{"id":"2su99n6iiik37iiknmb5t4fkfh_20260127T153000Z","iCalUID":"shared@example.org","start":{"dateTime":"2026-01-27T15:30:00Z"},"end":{"dateTime":"2026-01-27T16:30:00Z"}}`, "shared@example.com")
	for _, item := range []struct {
		id      int64
		mailbox string
	}{{own, "owner@example.com"}, {shared, "shared@example.com"}} {
		_, err := st.db.Exec(`UPDATE sources SET sync_config = ? WHERE id = (SELECT source_id FROM messages WHERE id = ?)`, fmt.Sprintf(`{"calendar_id":%q,"account_email":"accessor@example.com"}`, item.mailbox), item.id)
		require.NoError(err)
	}
	_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"2su99n6iiik37iiknmb5t4fkfh_20260127T153000Z","scheduled_start_time":"2026-01-27T15:30:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	require.Len(record.Links, 1)
	assert.Equal(t, fmt.Sprintf("evt:%d", own), record.Links[0].Ref)
	for _, tc := range []struct {
		id   int64
		want []int64
	}{{own, []int64{note}}, {shared, []int64{}}} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: tc.id})
		require.NoError(err)
		assert.Equal(t, tc.want, record.MessageIDs)
	}
}

func TestMeetingRecordDerivedGoogleAlias(t *testing.T) {
	for _, tc := range []struct{ name, start, end, original, movedStart, movedEnd, key string }{
		{"timed", `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-14T11:00:00Z"}`, `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-15T10:00:00Z"}`, `{"dateTime":"2027-03-15T11:00:00Z"}`, "20270314T100000Z"},
		{"all-day", `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, `{"date":"2027-03-16"}`, "20270314"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"series","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, tc.start, tc.end), "owner@example.com")
			_, edit := calendarFixture(t, st, "edit", fmt.Sprintf(`{"id":"edit","recurringEventId":"series","originalStartTime":%s,"start":%s,"end":%s}`, tc.original, tc.movedStart, tc.movedEnd), "owner@example.com")
			_, note := projectionFixture(t, st, "derived", "granola_json", fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series_%s"}}`, tc.key))
			for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: edit}, {Kind: "msg", MessageID: note}} {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(edit, record.Links[0].MessageID)
				assert.Equal([]int64{note}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingRecordFollowsAliasesAcrossCalendarCopies(t *testing.T) {
	for _, tc := range []struct {
		name, format, raw string
		partial           bool
	}{
		{"granola", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"first"}}`, false},
		{"twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, false},
		{"partial", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			firstRaw := `{"id":"first","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			secondRaw := `{"id":"second","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			if tc.partial {
				firstRaw = `{"id":"first","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`
				secondRaw = `{"id":"second","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			}
			_, first := calendarFixture(t, st, "first-copy", firstRaw, "owner@example.com")
			_, second := calendarFixture(t, st, "second-copy", secondRaw, "other@example.com")
			_, note := projectionFixture(t, st, "note-copy", tc.format, tc.raw)
			seeds := []MeetingLinkRef{{Kind: "msg", MessageID: note}}
			if !tc.partial {
				seeds = append(seeds, MeetingLinkRef{Kind: "evt", MessageID: first}, MeetingLinkRef{Kind: "evt", MessageID: second})
			}
			for _, seed := range seeds {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 2)
				assert.Equal(fmt.Sprintf("evt:%d", first), record.Links[0].Ref)
				assert.Equal(fmt.Sprintf("evt:%d", second), record.Links[1].Ref)
				if tc.partial {
					assert.Nil(record.Links[0].Start)
					assert.Nil(record.Links[0].End)
					assert.NotNil(record.Links[1].Start)
					assert.NotNil(record.Links[1].End)
				}
				assert.Equal([]int64{note}, record.MessageIDs)
			}
		})
	}
}
func TestMeetingRecordAllDayCopiesAcrossTimeZones(t *testing.T) {
	for _, recurring := range []bool{false, true} {
		name := "standalone"
		if recurring {
			name = "recurring"
		}
		t.Run(name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			recurrence := ""
			if recurring {
				recurrence = `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]`
			}
			firstData, first := calendarFixture(t, st, "ny", fmt.Sprintf(`{"id":"ny","iCalUID":"uid@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}%s}`, recurrence), "ny@example.com")
			secondData, second := calendarFixture(t, st, "tokyo", fmt.Sprintf(`{"id":"tokyo","iCalUID":"uid@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}%s}`, recurrence), "tokyo@example.com")
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T05:00:00Z"}}`)
			seeds := []MeetingLinkRef{{Kind: "msg", MessageID: note}}
			suffix := ""
			if recurring {
				_, firstMoved := calendarFixture(t, st, "ny-moved", `{"id":"ny-moved","iCalUID":"uid@example.org","recurringEventId":"ny","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`, "ny@example.com")
				_, secondMoved := calendarFixture(t, st, "tokyo-moved", `{"id":"tokyo-moved","iCalUID":"uid@example.org","recurringEventId":"tokyo","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`, "tokyo@example.com")
				seeds = append(seeds, MeetingLinkRef{Kind: "evt", MessageID: first, Identity: occurrenceIdentity(t, "20270314")}, MeetingLinkRef{Kind: "evt", MessageID: second, Identity: occurrenceIdentity(t, "20270314")}, MeetingLinkRef{Kind: "evt", MessageID: firstMoved}, MeetingLinkRef{Kind: "evt", MessageID: secondMoved})
				suffix = "@20270314"
			} else {
				seeds = append(seeds, MeetingLinkRef{Kind: "evt", MessageID: first}, MeetingLinkRef{Kind: "evt", MessageID: second})
			}
			require.NoError(st.UpdateSourceSyncConfig(t.Context(), firstData.Message.SourceID, `{"calendar_id":"ny@example.com","time_zone":"America/New_York"}`))
			require.NoError(st.UpdateSourceSyncConfig(t.Context(), secondData.Message.SourceID, `{"calendar_id":"tokyo@example.com","time_zone":"Asia/Tokyo"}`))
			for _, seed := range seeds {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 2)
				assert.Equal(fmt.Sprintf("evt:%d%s", second, suffix), record.Links[0].Ref)
				assert.Equal(fmt.Sprintf("evt:%d%s", first, suffix), record.Links[1].Ref)
				assert.Equal([]int64{note}, record.MessageIDs)
			}
			if recurring {
				next, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: first, Identity: occurrenceIdentity(t, "20270321")})
				require.NoError(err)
				require.Len(next.Links, 2)
				assert.Empty(next.MessageIDs)
			}
		})
	}
}

func TestMeetingRecordSkipsUnrelatedExceptionsWithinBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, master := calendarFixture(t, st, "master-budget", `{"id":"cap-series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "exception", 150)
	_, err := st.db.Exec(`UPDATE calendar_events SET ical_uid = 'uid@example.org' WHERE kind = 'exception'`)
	require.NoError(err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer release()
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	key := canonicalMeetingKey("ical_uid", "", "uid@example.org", &at)
	budget := 100
	var unexpandable []CalendarUnexpandable
	matches, err := st.meetingOccurrencesForKeys(ctx, []meetingLinkKey{key}, &budget, &unexpandable)
	require.NoError(err)
	require.Len(matches[key], 1)
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), matches[key][0].Ref)
}

func TestMeetingRecordEndOnlyMatchesCompleteCopy(t *testing.T) {
	for _, tc := range []struct{ name, start, otherStart, end, otherEnd string }{
		{"timed", `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-14T09:00:00Z"}`, `{"dateTime":"2027-03-14T11:00:00Z"}`, `{"dateTime":"2027-03-21T11:00:00Z"}`},
		{"all-day", `{"date":"2027-03-14"}`, `{"date":"2027-03-13"}`, `{"date":"2027-03-15"}`, `{"date":"2027-03-22"}`},
	} {
		for _, differentEnd := range []bool{false, true} {
			name := tc.name + "/same-end"
			if differentEnd {
				name = tc.name + "/different-end"
			}
			t.Run(name, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				data, complete := calendarFixture(t, st, "complete", fmt.Sprintf(`{"id":"event","start":%s,"end":%s}`, tc.start, tc.end), "first@example.com")
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
				data, partial := calendarFixture(t, st, "partial", fmt.Sprintf(`{"id":"event","status":"cancelled","end":%s}`, tc.end), "second@example.com")
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
				_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event"}}`)
				for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: complete}, {Kind: "evt", MessageID: partial}, {Kind: "msg", MessageID: note}} {
					record, err := st.MeetingRecordContext(t.Context(), ref)
					require.NoError(err)
					require.Len(record.Links, 2)
					assert.ElementsMatch([]string{fmt.Sprintf("evt:%d", complete), fmt.Sprintf("evt:%d", partial)}, []string{record.Links[0].Ref, record.Links[1].Ref})
					assert.Equal([]int64{note}, record.MessageIDs)
					assert.Contains(record.Unexpandable, CalendarUnexpandable{MessageID: partial, Reason: "missing event bounds"})
				}
				if !differentEnd {
					data, _ = calendarFixture(t, st, "other", fmt.Sprintf(`{"id":"event","start":%s,"end":%s}`, tc.otherStart, tc.end), "third@example.com")
					require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
					record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
					require.NoError(err)
					assert.Equal([]int64{note}, record.MessageIDs)
					assert.Empty(record.Links)
					return
				}
				data, second := calendarFixture(t, st, "second-partial", fmt.Sprintf(`{"id":"event","status":"cancelled","end":%s}`, tc.otherEnd), "fourth@example.com")
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
				require.NoError(err)
				assert.Equal([]int64{note}, record.MessageIDs)
				assert.Empty(record.Links)
				for _, id := range []int64{partial, second} {
					record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
					require.NoError(err)
					require.Len(record.Links, 1)
					assert.Equal(fmt.Sprintf("evt:%d", id), record.Links[0].Ref)
					assert.Empty(record.MessageIDs)
				}
			})
		}
	}
}

func TestMeetingRecordSeriesNotesByOwner(t *testing.T) {
	for _, tc := range []struct {
		name, mode       string
		copy, otherOwner bool
	}{
		{"same owner across accounts", "", true, false},
		{"different owner", "", true, true},
		{"untimed cancellation without master", "untimed", true, false},
		{"two timed copies", "two copies", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			masterOwner := "owner@example.com"
			if tc.copy {
				masterOwner = "second@example.com"
			}
			if tc.otherOwner {
				masterOwner = "other@example.com"
			}
			var data *MessagePersistData
			var master, copyID int64
			if tc.mode == "untimed" {
				_, copyID = calendarFixture(t, st, "cancelled", `{"id":"series","status":"cancelled"}`, "owner@example.com")
			} else if tc.mode != "two copies" {
				data, master = calendarFixture(t, st, "master", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, masterOwner)
			}
			if tc.copy && !tc.otherOwner && tc.mode == "" {
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"OWNER@example.com","account_email":"second@example.com","time_zone":"UTC"}`))
				data, copyID = calendarFixture(t, st, "sparse", `{"id":"series","status":"cancelled","end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "first@example.com")
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","account_email":"first@example.com","time_zone":"UTC"}`))
			} else if tc.otherOwner {
				_, copyID = calendarFixture(t, st, "standalone", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "owner@example.com")
			}
			_, first := projectionFixture(t, st, "week-one", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
			if tc.otherOwner {
				for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: copyID}, {Kind: "msg", MessageID: first}} {
					record, err := st.MeetingRecordContext(t.Context(), seed)
					require.NoError(err)
					require.Len(record.Links, 1)
					assert.Equal(fmt.Sprintf("evt:%d", copyID), record.Links[0].Ref)
					assert.Equal([]int64{first}, record.MessageIDs)
				}
				return
			}
			_, second := projectionFixture(t, st, "week-two", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
			for _, id := range []int64{first, second} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
				require.NoError(err)
				assert.Equal([]int64{id}, record.MessageIDs)
				assert.Empty(record.Links)
			}
			if tc.mode == "two copies" {
				data, event := calendarFixture(t, st, "first-event", `{"id":"series","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "first@example.com")
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","account_email":"first@example.com","time_zone":"UTC"}`))
				for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: first}, {Kind: "msg", MessageID: second}, {Kind: "evt", MessageID: event}} {
					record, err := st.MeetingRecordContext(t.Context(), seed)
					require.NoError(err)
					require.Len(record.Links, 1)
					assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
					assert.Equal([]int64{first, second}, record.MessageIDs)
				}
				data, _ = calendarFixture(t, st, "second-event", `{"id":"series","start":{"dateTime":"2027-03-21T12:00:00Z"},"end":{"dateTime":"2027-03-21T13:00:00Z"}}`, "second@example.com")
				require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","account_email":"second@example.com","time_zone":"UTC"}`))
				for _, id := range []int64{first, second} {
					record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
					require.NoError(err)
					assert.Equal([]int64{id}, record.MessageIDs)
					assert.Empty(record.Links)
				}
				return
			}
			if tc.copy {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: copyID})
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d", copyID), record.Links[0].Ref)
				assert.Nil(record.Links[0].Start)
				if tc.mode == "untimed" {
					assert.Nil(record.Links[0].End)
				} else {
					assert.NotNil(record.Links[0].End)
				}
				assert.Empty(record.MessageIDs)
				assert.Contains(record.Unexpandable, CalendarUnexpandable{MessageID: copyID, Reason: "missing event bounds"})
			} else {
				for _, key := range []string{"20270314T100000Z", "20270321T100000Z"} {
					record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, key)})
					require.NoError(err)
					require.Len(record.Links, 1)
					assert.Equal(fmt.Sprintf("evt:%d@%s", master, key), record.Links[0].Ref)
					assert.Empty(record.MessageIDs)
				}
			}
		})
	}
}

func occurrenceIdentity(t *testing.T, key string) calrecur.Identity {
	t.Helper()
	_, identity, ok := calrecur.ParseInstanceID("event_" + key)
	require.True(t, ok)
	return identity
}

func TestUniqueMeetingOccurrencesOrder(t *testing.T) {
	start := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	items := []CalendarOccurrence{{Ref: "evt:10", MessageID: 10, Start: start}, {Ref: "evt:9", MessageID: 9, Start: start}, {Ref: "evt:11", MessageID: 11, Start: start.Add(-time.Hour)}, {Ref: "evt:9", MessageID: 9, Start: start}, {Ref: "evt:1", MessageID: 1}}
	got := uniqueMeetingOccurrences(items)
	require.Len(t, got, 4)
	assert.Equal(t, []string{"evt:1", "evt:11", "evt:9", "evt:10"}, []string{got[0].Ref, got[1].Ref, got[2].Ref, got[3].Ref})
}

func TestMeetingRecordNoteExpansionExhaustsSharedBudget(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 10 {
		calendarFixture(t, st, fmt.Sprintf("old-series-%d", i), fmt.Sprintf(`{"id":"series-%d","iCalUID":"budget@example.org","start":{"dateTime":"1880-01-01T10:00:00Z"},"end":{"dateTime":"1880-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, i))
	}
	raw := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"budget@example.org","startsAt":"2027-03-14T12:00:00Z"}}`
	_, note := projectionFixture(t, st, "budget-note", "twenty_json", raw)
	_, copyID := projectionFixture(t, st, "budget-copy", "twenty_json", raw)
	_, err := st.db.ExecContext(t.Context(), `UPDATE messages SET sent_at = ? WHERE id IN (?, ?)`, time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC), note, copyID)
	require.NoError(t, err)
	_, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.ErrorIs(t, err, ErrQueryTooBroad)
}

func TestMeetingRecordRetainsRecurrenceBudgetWarning(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "long-series", `{"id":"series","iCalUID":"long@example.org","start":{"dateTime":"1880-01-01T10:00:00Z"},"end":{"dateTime":"1880-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=60000"]}`)
	_, note := projectionFixture(t, st, "long-series-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"long@example.org","startsAt":"2027-01-01T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(t, err)
	assert.Contains(t, record.Unexpandable, CalendarUnexpandable{MessageID: series, Reason: calrecur.ErrBudgetExceeded.Error()})
}

func TestMeetingRecordAllDayMasterWithoutZone(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("gcal", "unknown-zone@example.com")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"calendar_id":"unknown-zone@example.com"}`))
	var edit int64
	for i, raw := range []string{
		`{"id":"series","iCalUID":"zone@example.org","start":{"date":"2027-03-02"},"end":{"date":"2027-03-03"},"recurrence":["RRULE:FREQ=DAILY;COUNT=20"]}`,
		`{"id":"edit","iCalUID":"zone@example.org","recurringEventId":"series","originalStartTime":{"date":"2027-03-04"},"start":{"date":"2027-03-06"},"end":{"date":"2027-03-07"}}`,
	} {
		id := fmt.Sprintf("unknown-zone-%d", i)
		data := &MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: id, MessageType: "calendar_event", Subject: sql.NullString{String: "Example event", Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: id, ConversationType: "calendar", Title: "Example event"},
			RawMIME:      []byte(raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"unknown-zone@example.com"}`, Valid: true},
		}
		edit, err = st.PersistMessage(data)
		require.NoError(t, err)
	}
	_, note := projectionFixture(t, st, "unknown-zone-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zone@example.org","startsAt":"2027-03-04T00:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: edit})
	require.NoError(t, err)
	require.Len(t, record.Links, 1)
	assert.Empty(t, record.MessageIDs)
	record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(t, err)
	assert.Empty(t, record.Links)
}
