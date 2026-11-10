package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingCountKeysSingleSplitAfterCalendarCancellationOrDeletion(t *testing.T) {
	for _, change := range []string{"cancelled", "deleted from source"} {
		t.Run(change, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, event := calendarFixture(t, st, "single", singleLinkRaw("single", "single@example.org"))
			var notes []int64
			for _, name := range []string{"first", "second"} {
				_, note := projectionFixture(t, st, name, "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"single@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
				notes = append(notes, note)
			}
			before, err := st.MeetingCountKeysContext(t.Context())
			require.NoError(err)
			assert.Equal(map[int64][]string{notes[0]: {"uid:single@example.org/"}, notes[1]: {"uid:single@example.org/"}}, before)
			require.NoError(st.UnlinkMeetingContext(t.Context(), notes[0], OccurrenceRef{MessageID: event}))
			if change == "cancelled" {
				require.NoError(st.SetMessageMetadataContext(t.Context(), event, sql.NullString{String: `{"calendar_id":"calendar@example.com","ical_uid":"single@example.org","status":"cancelled"}`, Valid: true}))
			} else {
				require.NoError(st.MarkMessageDeletedContext(t.Context(), data.Message.SourceID, "single"))
			}
			keys, err := st.MeetingCountKeysContext(t.Context())
			require.NoError(err)
			assert.Empty(keys[notes[0]])
			assert.Equal([]string{"uid:single@example.org/"}, keys[notes[1]])
		})
	}
}

func TestMeetingCountKeysOneTranscriptTwoOccurrences(t *testing.T) {
	type note struct{ format, raw string }
	type step struct {
		seriesRaw, exceptionRaw, splitKey string
		want                              []string
	}
	const twentyRaw = `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T10:00:00Z"}}`
	const movedRaw = `{"id":"moved","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":%q},"end":{"dateTime":%q}}`
	plain := []note{{"plaud_json", `{}`}}
	copies := []note{
		{"granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z","scheduled_start_time":"2027-03-21T10:00:00Z"}}`},
		{"twenty_json", twentyRaw},
	}
	shortened := strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1)
	unsupported := strings.Replace(linkSeriesRaw, "FREQ=WEEKLY;COUNT=6", "FREQ=YEARLY;BYWEEKNO=20", 1)
	for _, tc := range []struct {
		name      string
		notes     []note
		links     []string
		exception bool
		steps     []step
	}{
		{name: "manual", notes: plain, links: []string{"20270314T100000Z", "20270321T100000Z"}, steps: []step{
			{seriesRaw: linkSeriesRaw, want: []string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: shortened, want: []string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: unsupported, want: []string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
		}},
		{name: "automatic", notes: copies, steps: []step{
			{seriesRaw: linkSeriesRaw, want: []string{"uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: shortened, want: []string{"uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: unsupported, want: []string{"uid:weekly@example.org/20270321T100000Z"}},
		}},
		{name: "mixed removed link", notes: copies, links: []string{"20270314T100000Z"}, steps: []step{
			{seriesRaw: linkSeriesRaw, want: []string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: shortened, want: []string{"uid:weekly@example.org/20270314T100000Z"}},
			{seriesRaw: unsupported, want: []string{"uid:weekly@example.org/20270314T100000Z"}},
		}},
		{name: "mixed surviving link", notes: copies, links: []string{"20270307T100000Z"}, steps: []step{
			{seriesRaw: linkSeriesRaw, want: []string{"uid:weekly@example.org/20270307T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: shortened, want: []string{"uid:weekly@example.org/20270307T100000Z"}},
			{seriesRaw: unsupported, want: []string{"uid:weekly@example.org/20270307T100000Z"}},
		}},
		{name: "moved exception", notes: plain, links: []string{"20270314T100000Z"}, exception: true, steps: []step{
			{want: []string{"uid:weekly@example.org/20270314T100000Z"}},
			{exceptionRaw: fmt.Sprintf(movedRaw, "2027-03-15T14:00:00Z", "2027-03-15T15:00:00Z"), want: []string{"uid:weekly@example.org/20270314T100000Z"}},
		}},
		{name: "split", notes: []note{{"twenty_json", twentyRaw}}, steps: []step{
			{seriesRaw: shortened, splitKey: "20270307T100000Z", want: []string{"uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: shortened, splitKey: "20270321T100000Z"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "series", linkSeriesRaw)
			var exception int64
			if tc.exception {
				_, exception = calendarFixture(t, st, "moved", fmt.Sprintf(movedRaw, "2027-03-14T12:00:00Z", "2027-03-14T13:00:00Z"))
			}
			var notes []int64
			for i, data := range tc.notes {
				_, id := projectionFixture(t, st, fmt.Sprintf("note-%d", i), data.format, data.raw)
				notes = append(notes, id)
				for _, key := range tc.links {
					require.NoError(st.LinkMeetingContext(t.Context(), id, OccurrenceRef{MessageID: master, Key: key}))
				}
			}
			for _, change := range tc.steps {
				for _, id := range notes {
					if change.splitKey != "" {
						require.NoError(st.UnlinkMeetingContext(t.Context(), id, OccurrenceRef{MessageID: master, Key: change.splitKey}))
					}
				}
				if change.seriesRaw != "" {
					require.NoError(st.UpsertMessageRawWithFormat(master, []byte(change.seriesRaw), "gcal_json"))
				}
				if change.exceptionRaw != "" {
					require.NoError(st.UpsertMessageRawWithFormat(exception, []byte(change.exceptionRaw), "gcal_json"))
				}
				keys, err := st.MeetingCountKeysContext(t.Context())
				require.NoError(err)
				for _, id := range notes {
					assert.ElementsMatch(change.want, keys[id])
				}
				if change.splitKey != "" {
					require.NoError(st.UpsertMessageRawWithFormat(master, []byte(linkSeriesRaw), "gcal_json"))
				}
			}
		})
	}
}

func TestMeetingCountKeysAfterRemovingRecurrence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	_, manual := projectionFixture(t, st, "manual", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), manual, OccurrenceRef{MessageID: master, Key: "20270307T100000Z"}))
	_, twenty := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-07T10:00:00Z"}}`)
	_, granola := projectionFixture(t, st, "granola", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270307T100000Z","scheduled_start_time":"2027-03-07T10:00:00Z"}}`)
	require.NoError(st.UpsertMessageRawWithFormat(master, []byte(strings.Replace(linkSeriesRaw, `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]`, "", 1)), "gcal_json"))
	keys, err := st.MeetingCountKeysContext(t.Context())
	require.NoError(err)
	for _, note := range []int64{manual, twenty, granola} {
		assert.Equal([]string{"uid:weekly@example.org/"}, keys[note])
	}
}

func TestMeetingCountKeysExactEvidenceWithoutCalendar(t *testing.T) {
	for _, tc := range []struct {
		name, format, first, second string
		want                        []string
	}{
		{"UID and instant", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T10:00:00Z"}}`, `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T06:00:00-04:00"}}`, []string{"uid:weekly@example.org/20270321T100000Z"}},
		{"UID alone", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org"}}`, "", nil},
		{"Google instance", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z"}}`, "", []string{`gcal:"series"/20270321T100000Z`}},
		{"Google series and instant", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`, `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z"}}`, []string{`gcal:"series"/20270321T100000Z`}},
		{"Google series alone", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series"}}`, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			for i, raw := range []string{tc.first, tc.second} {
				if raw == "" {
					continue
				}
				_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), tc.format, raw)
				keys, err := st.MeetingCountKeysContext(t.Context())
				require.NoError(err)
				assert.Equal(tc.want, keys[note])
			}
		})
	}
}

func TestMeetingCountKeysGoogleAcrossMailboxes(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "series", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1))
	for _, mailbox := range []string{"calendar@example.com", "other@example.com"} {
		_, note := projectionFixture(t, st, mailbox, "granola_json", fmt.Sprintf(`{"owner":{"email":%q},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`, mailbox))
		keys, err := st.MeetingCountKeysContext(t.Context())
		require.NoError(err)
		assert.Equal([]string{"uid:weekly@example.org/20270321T100000Z"}, keys[note])
	}
	calendarFixture(t, st, "single", singleLinkRaw("single", "single@example.org"))
	_, note := projectionFixture(t, st, "single", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"single","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	keys, err := st.MeetingCountKeysContext(t.Context())
	require.NoError(err)
	assert.Equal([]string{"uid:single@example.org/"}, keys[note])
}

func TestMeetingCountKeysRejectsBroadEvidence(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		id := fmt.Sprintf("copy-%d", i)
		calendarFixture(t, st, id, singleLinkRaw(id, "crowded@example.org"))
	}
	projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, err := st.MeetingCountKeysContext(t.Context())
	require.ErrorIs(t, err, ErrQueryTooBroad)
}
