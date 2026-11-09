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

func TestMeetingCountKeysSavedLinkSuppressesUnmatchedEvidence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270307T100000Z"}))
	keys, err := st.MeetingCountKeysContext(t.Context())
	require.NoError(err)
	assert.Equal([]string{"uid:weekly@example.org/20270307T100000Z"}, keys[note])
}

func TestMeetingCountKeysOneTranscriptTwoOccurrences(t *testing.T) {
	for _, mode := range []string{"manual", "automatic", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "series", linkSeriesRaw)
			var notes []int64
			if mode == "manual" {
				_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
				notes = append(notes, note)
			} else {
				_, google := projectionFixture(t, st, "google", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
				_, twenty := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
				notes = append(notes, google, twenty)
			}
			want := []string{"uid:weekly@example.org/20270321T100000Z"}
			if mode != "automatic" {
				want = []string{"uid:weekly@example.org/20270314T100000Z", "uid:weekly@example.org/20270321T100000Z"}
				for _, note := range notes {
					require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
					if mode == "manual" {
						require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270321T100000Z"}))
					}
				}
			}
			for step, raw := range []string{linkSeriesRaw, strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1), strings.Replace(linkSeriesRaw, "FREQ=WEEKLY;COUNT=6", "FREQ=YEARLY;BYWEEKNO=20", 1)} {
				if mode == "mixed" && step > 0 {
					want = want[:1]
				}
				require.NoError(st.UpsertMessageRawWithFormat(master, []byte(raw), "gcal_json"))
				keys, err := st.MeetingCountKeysContext(t.Context())
				require.NoError(err)
				for _, note := range notes {
					assert.ElementsMatch(want, keys[note])
				}
				assert.NotContains(keys, master)
			}
		})
	}
}
func TestMeetingCountKeysMovedExceptionKeepsOccurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	raw := `{"id":"moved","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":%q},"end":{"dateTime":%q}}`
	_, exception := calendarFixture(t, st, "moved", fmt.Sprintf(raw, "2027-03-14T12:00:00Z", "2027-03-14T13:00:00Z"))
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	before, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	requirements.Equal([]string{"uid:weekly@example.org/20270314T100000Z"}, before[note])
	requirements.NoError(st.UpsertMessageRawWithFormat(exception, []byte(fmt.Sprintf(raw, "2027-03-15T14:00:00Z", "2027-03-15T15:00:00Z")), "gcal_json"))
	after, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(before[note], after[note])
	assertions.NotContains(after, master)
	assertions.NotContains(after, exception)
}

func TestMeetingCountKeysExactEvidenceWithoutCalendar(t *testing.T) {
	for _, tc := range []struct {
		name, format, first, second string
		want                        []string
	}{
		{"UID and instant", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T10:00:00Z"}}`, `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T06:00:00-04:00"}}`, []string{"uid:weekly@example.org/20270321T100000Z"}},
		{"UID alone", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org"}}`, "", nil},
		{"Google instance", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z"}}`, "", []string{`gcal:"calendar@example.com"/"series"/20270321T100000Z`}},
		{"Google series and instant", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`, `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z"}}`, []string{`gcal:"calendar@example.com"/"series"/20270321T100000Z`}},
		{"Google series alone", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series"}}`, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			if tc.second == "" {
				tc.second = tc.first
			}
			for i, raw := range []string{tc.first, tc.second} {
				_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), tc.format, raw)
				keys, err := st.MeetingCountKeysContext(t.Context())
				require.NoError(err)
				assert.Equal(tc.want, keys[note])
			}
		})
	}
}

func TestMeetingCountKeysSplitNamesOnlyItsOccurrence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	for _, key := range []string{"20270307T100000Z", "20270321T100000Z"} {
		require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: key}))
		require.NoError(st.UpsertMessageRawWithFormat(master, []byte(strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1)), "gcal_json"))
		keys, err := st.MeetingCountKeysContext(t.Context())
		require.NoError(err)
		if key == "20270307T100000Z" {
			assert.Equal([]string{"uid:weekly@example.org/20270321T100000Z"}, keys[note])
		} else {
			assert.Empty(keys[note])
		}
		require.NoError(st.UpsertMessageRawWithFormat(master, []byte(linkSeriesRaw), "gcal_json"))
	}
}

func TestMeetingCountKeysGoogleMailboxScope(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "series", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1))
	for _, mailbox := range []string{"calendar@example.com", "other@example.com"} {
		_, note := projectionFixture(t, st, mailbox, "granola_json", fmt.Sprintf(`{"owner":{"email":%q},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`, mailbox))
		keys, err := st.MeetingCountKeysContext(t.Context())
		require.NoError(err)
		want := "uid:weekly@example.org/20270321T100000Z"
		if mailbox == "other@example.com" {
			want = `gcal:"other@example.com"/"series"/20270321T100000Z`
		}
		assert.Equal([]string{want}, keys[note])
	}
	calendarFixture(t, st, "single", singleLinkRaw("single", "single@example.org"))
	_, note := projectionFixture(t, st, "single", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"single","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	keys, err := st.MeetingCountKeysContext(t.Context())
	require.NoError(err)
	assert.Equal([]string{"uid:single@example.org/"}, keys[note])
}
