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
			before, err := meetingCountTestKeys(t, st)
			require.NoError(err)
			assert.Equal(map[int64][]string{notes[0]: {"uid:single@example.org/"}, notes[1]: {"uid:single@example.org/"}}, before)
			require.NoError(st.UnlinkMeetingContext(t.Context(), notes[0], OccurrenceRef{MessageID: event}))
			if change == "cancelled" {
				require.NoError(st.SetMessageMetadataContext(t.Context(), event, sql.NullString{String: `{"calendar_id":"calendar@example.com","ical_uid":"single@example.org","status":"cancelled"}`, Valid: true}))
			} else {
				require.NoError(st.MarkMessageDeletedContext(t.Context(), data.Message.SourceID, "single"))
			}
			keys, err := meetingCountTestKeys(t, st)
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
			{seriesRaw: shortened, want: []string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: unsupported, want: []string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
		}},
		{name: "mixed surviving link", notes: copies, links: []string{"20270307T100000Z"}, steps: []step{
			{seriesRaw: linkSeriesRaw, want: []string{"uid:weekly@example.org/20270307T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: shortened, want: []string{"uid:weekly@example.org/20270307T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
			{seriesRaw: unsupported, want: []string{"uid:weekly@example.org/20270307T100000Z", "uid:weekly@example.org/20270321T100000Z"}},
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
				keys, err := meetingCountTestKeys(t, st)
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
	keys, err := meetingCountTestKeys(t, st)
	require.NoError(err)
	for _, note := range []int64{manual, twenty, granola} {
		assert.Equal([]string{"uid:weekly@example.org/"}, keys[note])
	}
}

func TestMeetingCountKeysGoogleAcrossMailboxes(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "series", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1))
	for _, mailbox := range []string{"calendar@example.com", "other@example.com"} {
		_, note := projectionFixture(t, st, mailbox, "granola_json", fmt.Sprintf(`{"owner":{"email":%q},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`, mailbox))
		keys, err := meetingCountTestKeys(t, st)
		require.NoError(err)
		assert.Equal([]string{"uid:weekly@example.org/20270321T100000Z"}, keys[note])
	}
	calendarFixture(t, st, "single", singleLinkRaw("single", "single@example.org"))
	_, note := projectionFixture(t, st, "single", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"single","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	keys, err := meetingCountTestKeys(t, st)
	require.NoError(err)
	assert.Equal([]string{"uid:single@example.org/"}, keys[note])
}

func TestMeetingCountKeysManyCalendarCopies(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		id := fmt.Sprintf("copy-%d", i)
		calendarFixture(t, st, id, singleLinkRaw(id, "crowded@example.org"))
	}
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	keys, err := meetingCountTestKeys(t, st)
	require.NoError(t, err)
	assert.Equal(t, []string{"uid:crowded@example.org/"}, keys[note])
}

func TestMeetingCountKeysBudgetExhaustionKeepsOtherMessages(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for _, id := range []string{"crowded-1", "crowded-2"} {
		calendarFixture(t, st, id, singleLinkRaw(id, "crowded@example.org"))
	}
	calendarFixture(t, st, "other", singleLinkRaw("other", "other@example.org"))
	_, crowded := projectionFixture(t, st, "crowded", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, other := projectionFixture(t, st, "other", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"other@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	keys, err := st.meetingCountKeysContext(t.Context(), 3)
	require.NoError(t, err)
	assert.Empty(t, keys[crowded], "the cache uses the transcript's own key")
	require.Len(t, keys[other], 1)
	assert.Equal(t, "uid:other@example.org/", keys[other][0].Key)
}

func TestMeetingCountKeysGoogleCalendarCollisions(t *testing.T) {
	type event struct{ uid, kind string }
	for _, tc := range []struct {
		name   string
		events []event
		want   []string
	}{
		{name: "no calendar"},
		{name: "one calendar", events: []event{{"first@example.org", "single"}}, want: []string{"uid:first@example.org/"}},
		{name: "same occurrence", events: []event{{"first@example.org", "single"}, {"first@example.org", "single"}}, want: []string{"uid:first@example.org/"}},
		{name: "different singles", events: []event{{"first@example.org", "single"}, {"second@example.org", "single"}}},
		{name: "different series", events: []event{{"first@example.org", "series"}, {"second@example.org", "series"}}},
		{name: "different single and series", events: []event{{"first@example.org", "single"}, {"second@example.org", "series"}}},
		{name: "single without UID and series", events: []event{{"", "single"}, {"second@example.org", "series"}}},
		{name: "single and series without UID", events: []event{{"first@example.org", "single"}, {"", "series"}}},
		{name: "same series", events: []event{{"first@example.org", "series"}, {"first@example.org", "series"}}, want: []string{"uid:first@example.org/20270314T100000Z"}},
		{name: "same single and series", events: []event{{"first@example.org", "single"}, {"first@example.org", "series"}}, want: []string{"uid:first@example.org/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			for i, event := range tc.events {
				raw := singleLinkRaw("event", event.uid)
				if event.kind == "series" {
					raw = fmt.Sprintf(`{"id":"event","iCalUID":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, event.uid)
				}
				calendarFixture(t, st, fmt.Sprintf("event-%d", i), raw, fmt.Sprintf("calendar-%d@example.com", i))
			}
			var notes []int64
			for i := range 2 {
				_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "granola_json", `{"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
				notes = append(notes, note)
			}
			keys, err := meetingCountTestKeys(t, st)
			require.NoError(t, err)
			for _, note := range notes {
				assert.Equal(t, tc.want, keys[note])
			}
		})
	}
}

func TestMeetingCountKeysMovedCalendarCopy(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	calendarFixture(t, st, "moved", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`, "copy@example.com")
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-15T12:00:00Z"}}`)
	keys, err := meetingCountTestKeys(t, st)
	require.NoError(t, err)
	assert.Equal(t, []string{"uid:weekly@example.org/20270314T100000Z"}, keys[note])
}

func TestMeetingCountKeysGoogleExceptionWithoutUID(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "moved", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "granola_json", `{"calendar_event":{"calendar_event_id":"series_20270314T100000Z","scheduled_start_time":"2027-03-15T12:00:00Z"}}`)
	keys, err := meetingCountTestKeys(t, st)
	require.NoError(t, err)
	assert.Equal(t, []string{"uid:weekly@example.org/20270314T100000Z"}, keys[note])
}

func meetingCountTestKeys(t *testing.T, st *Store) (map[int64][]string, error) {
	t.Helper()
	occurrences, err := st.MeetingCountKeysContext(t.Context())
	keys := make(map[int64][]string, len(occurrences))
	for id, values := range occurrences {
		for _, value := range values {
			keys[id] = append(keys[id], value.Key)
		}
	}
	return keys, err
}
