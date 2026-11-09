package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			for _, raw := range []string{linkSeriesRaw, strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1)} {
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

func TestMeetingCountKeysCalendarEdits(t *testing.T) {
	const eventRaw = `{"id":"series","iCalUID":%q,"start":{"dateTime":"2027-03-14T10:00:00","timeZone":%q},"end":{"dateTime":"2027-03-14T11:00:00","timeZone":%q},"recurrence":%s}`
	original := fmt.Sprintf(eventRaw, "series@example.test", "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3"]`)
	excluded := fmt.Sprintf(eventRaw, "series@example.test", "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3","EXDATE:20270321T100000Z"]`)
	zoned := fmt.Sprintf(eventRaw, "series@example.test", "America/New_York", "America/New_York", `["RRULE:FREQ=WEEKLY;COUNT=3"]`)
	for _, step := range []struct {
		name, beforeRaw, beforeKey, raw, wantKey string
	}{
		{"baseline", "", "", original, "uid:series@example.test/20270321T100000Z"},
		{"projection removed", "", "", `{}`, ""},
		{"restored after projection removal", `{}`, "", original, "uid:series@example.test/20270321T100000Z"},
		{"EXDATE", "", "", excluded, "uid:series@example.test/20270321T100000Z"},
		{"restored after EXDATE", excluded, "uid:series@example.test/20270321T100000Z", original, "uid:series@example.test/20270321T100000Z"},
		{"zone", "", "", zoned, "uid:series@example.test/20270321T100000Z"},
		{"restored after zone", zoned, "uid:series@example.test/20270321T100000Z", original, "uid:series@example.test/20270321T100000Z"},
		{"UID", "", "", fmt.Sprintf(eventRaw, "different@example.test", "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3"]`), ""},
	} {
		t.Run(step.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "series", original)
			var notes []int64
			for _, id := range []string{"first", "second"} {
				_, note := projectionFixture(t, st, id, "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"series@example.test","startsAt":"2027-03-21T10:00:00Z"}}`)
				notes = append(notes, note)
			}
			read := func(want string) {
				t.Helper()
				keys, err := st.MeetingCountKeysContext(t.Context())
				require.NoError(err)
				for _, note := range notes {
					if want == "" {
						assert.Empty(keys[note])
					} else {
						assert.Equal([]string{want}, keys[note])
					}
				}
			}
			read("uid:series@example.test/20270321T100000Z")
			if step.beforeRaw != "" {
				calendarFixture(t, st, "series", step.beforeRaw)
				read(step.beforeKey)
			}
			calendarFixture(t, st, "series", step.raw)
			read(step.wantKey)
		})
	}
}
