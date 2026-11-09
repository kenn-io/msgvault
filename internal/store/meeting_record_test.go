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

func repeatedMeetingSeriesFixture(t *testing.T, noteCount int) (*Store, int64, []int64) {
	t.Helper()
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"series","iCalUID":"daily@example.org","start":{"dateTime":"2010-01-01T10:00:00Z"},"end":{"dateTime":"2010-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`
	var event int64
	for i := range 5 {
		_, id := calendarFixture(t, st, fmt.Sprintf("calendar-%d", i), raw, fmt.Sprintf("calendar-%d@example.com", i))
		if i == 0 {
			event = id
		}
	}
	var notes []int64
	for i := range noteCount {
		_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"daily@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
		notes = append(notes, note)
	}
	return st, event, notes
}

func TestMeetingRecordRepeatedSeriesWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		notes int
	}{
		{"record", 7},
		{"manual link", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st, event, notes := repeatedMeetingSeriesFixture(t, tc.notes)
			r := st.meetingReader().withMeetingCache()
			budget := calrecur.RequestReadBudget
			target, err := resolveMeetingOccurrence(t.Context(), r, OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}, &budget)
			require.NoError(err)
			members, err := st.meetingOccurrenceMembers(t.Context(), r, target, &budget)
			// Five copies expanded and resolved once need fewer than 100,000 steps.
			assert.Less(calrecur.RequestReadBudget-budget, 100_000)
			require.NoError(err)
			assert.Equal(notes, members)
			if tc.name == "manual link" {
				_, note := projectionFixture(t, st, "manual", "plaud_json", `{}`)
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}))
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
				require.NoError(err)
				assert.Equal(append(notes, note), record.MessageIDs)
				return
			}
			for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: event, Key: "20270314T100000Z"}, {Kind: "msg", MessageID: notes[0]}} {
				record, err := st.MeetingRecordContext(t.Context(), ref)
				require.NoError(err, "record %s:%d", ref.Kind, ref.MessageID)
				assert.Equal(notes, record.MessageIDs)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", event), record.Links[0].Ref)
			}
		})
	}
}

func TestMeetingSuggestionsReuseSeriesWork(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st, event, notes := repeatedMeetingSeriesFixture(t, 10)
	ref := fmt.Sprintf("evt:%d@20270314T100000Z", event)
	var candidates []MeetingSuggestion
	for _, note := range notes {
		candidates = append(candidates, MeetingSuggestion{MemberMessageID: note, Ref: ref})
	}
	budget := calrecur.RequestReadBudget
	filtered, err := st.undecidedMeetingSuggestions(t.Context(), st.meetingReader().withMeetingCache(), candidates, 100, &budget)
	assert.Less(calrecur.RequestReadBudget-budget, 100_000)
	require.NoError(err)
	assert.Empty(filtered)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event, Key: "20270314T100000Z"}, 100)
	require.NoError(err)
	assert.Empty(page.Suggestions)
}

func TestMeetingRecordIncludesNotesOnMovedCopy(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	_, exception := calendarFixture(t, st, "moved", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`, "copy@example.com")
	var notes []int64
	for i, start := range []string{"2027-03-14T10:00:00Z", "2027-03-15T12:00:00Z"} {
		_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, start))
		notes = append(notes, note)
		links, err := meetingLinksForTest(t, st, note)
		requirements.NoError(err)
		requirements.Len(links, 1)
	}
	for _, ref := range []OccurrenceRef{{MessageID: master, Key: "20270314T100000Z"}, {MessageID: exception}} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: ref})
		requirements.NoError(err)
		assert.Equal(t, notes, record.MessageIDs)
	}
}

func TestMeetingRecordIncludesOrphanDecisionThroughSeriesCopy(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, exception := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")

	for _, phase := range []string{"before removal", "after removal"} {
		t.Run(phase, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			if phase == "after removal" {
				require.NoError(st.UpsertMessageRawWithFormat(master, []byte(singleLinkRaw("series", "weekly@example.org")), "gcal_json"))
			}
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
			require.NoError(err)
			require.Len(record.Links, 1)
			assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), record.Links[0].Ref)
			assert.Equal([]int64{note}, record.MessageIDs)

			record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"})
			require.NoError(err)
			assert.Equal([]int64{note}, record.MessageIDs)
		})
	}
}

func TestMeetingRecordMovedAllDayCopiesShareOriginalStartsAcrossZones(t *testing.T) {
	for _, tc := range []struct {
		name, start string
		moved       bool
	}{{"UTC midnight", "2027-03-14T00:00:00Z", false}, {"Tokyo midnight with moved exception", "2027-03-13T15:00:00Z", true}} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"series","iCalUID":"all-day@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
			_, utc := calendarFixture(t, st, "utc", raw)
			tokyoData, tokyo := calendarFixture(t, st, "tokyo", raw, "copy@example.com")
			require.NoError(st.UpdateSourceSyncConfig(tokyoData.Message.SourceID, `{"calendar_id":"copy@example.com","time_zone":"Asia/Tokyo"}`))
			_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"all-day@example.org","startsAt":%q}}`, tc.start))
			if tc.moved {
				wantRef := fmt.Sprintf("evt:%d@20270314", tokyo)
				require.Equal(wantRef, linkedMeetingRefs(t, st, note)[note])
				calendarFixture(t, st, "moved", `{"id":"series_20270314","iCalUID":"all-day@example.org","recurringEventId":"series","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`, "copy@example.com")
				require.NoError(st.UpdateSourceSyncConfig(tokyoData.Message.SourceID, `{"calendar_id":"copy@example.com","time_zone":"Asia/Tokyo"}`))
				require.Equal(wantRef, linkedMeetingRefs(t, st, note)[note])
			}
			for _, id := range []int64{tokyo, utc} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id, Key: "20270314"})
				require.NoError(err)
				assert.Equal([]int64{note}, record.MessageIDs, "calendar copy %d", id)
			}
		})
	}
}

func TestMeetingRecordRecurringGoogleEvidence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	projectionFixture(t, st, "other-note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal([]int64{note}, record.MessageIDs)
}

func TestMeetingDecisionsFollowMessageIdentity(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "old@example.org"))
	_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", "old@example.org"), "second@example.com")
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	var count int
	require.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
	assert.Equal(1, count)
	calendarFixture(t, st, "copy", strings.ReplaceAll(singleLinkRaw("copy", "old@example.org"), "old@example.org", "new@example.org"), "second@example.com")
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	assert.Empty(links)
}

func TestMeetingDecisionsAcrossRecurrenceChanges(t *testing.T) {
	for _, tc := range []struct {
		name, changed                        string
		copy, automatic, suggestions, orphan bool
	}{
		{"unlink unavailable", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1), false, false, false, false},
		{"unlink readable copy after budget", `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"1800-01-01T10:00:00Z"},"end":{"dateTime":"1800-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, true, false, false, false},
		{"replace unsupported copy", strings.Replace(linkSeriesRaw, "FREQ=WEEKLY", "FREQ=HOURLY", 1), true, true, false, false},
		{"split suppresses suggestions", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=1", 1), true, false, true, false},
		{"orphan split after recurrence removed", strings.Replace(linkSeriesRaw, `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]`, "", 1), false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var automatic, orphan int64
			if tc.automatic {
				_, automatic = calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
			}
			if tc.orphan {
				_, orphan = calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
			}
			format, raw := "plaud_json", `{}`
			if tc.automatic {
				format, raw = "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`
			}
			if tc.suggestions {
				format, raw = "circleback_json", `{"meeting":{"url":"https://meet.example.com/room"}}`
			}
			data, note := projectionFixture(t, st, "note", format, raw)
			if tc.suggestions {
				data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
				_, err := st.UpsertMessage(data.Message)
				require.NoError(err)
			}
			if tc.orphan {
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
			}
			_, master := calendarFixture(t, st, "master", linkSeriesRaw)
			ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
			target := ref
			if tc.copy {
				_, second := calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
				target.MessageID = second
			}
			refs := []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", OccurrenceRef: target}}
			if tc.suggestions {
				for _, r := range refs {
					page, err := st.SuggestMeetingLinksContext(t.Context(), r, 10)
					require.NoError(err)
					require.NotEmpty(page.Suggestions)
				}
			}
			if tc.suggestions || tc.orphan {
				require.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
			} else {
				require.NoError(st.LinkMeetingContext(t.Context(), note, ref))
			}
			calendarFixture(t, st, "master", tc.changed)
			if tc.suggestions {
				for _, r := range refs {
					page, err := st.SuggestMeetingLinksContext(t.Context(), r, 10)
					require.NoError(err)
					assert.Empty(page.Suggestions)
				}
				return
			}
			if !tc.copy && !tc.orphan {
				assert.Empty(linkedMeetingRefs(t, st, note)[note])
				require.Error(st.LinkMeetingContext(t.Context(), note, ref), "new links require an available occurrence")
			}
			if !tc.orphan {
				require.NoError(st.UnlinkMeetingContext(t.Context(), note, target))
				calendarFixture(t, st, "master", linkSeriesRaw)
			}
			links, err := meetingLinksForTest(t, st, note)
			require.NoError(err)
			if tc.automatic {
				require.Len(links, 1)
				assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: automatic}), links[0].Ref)
			} else {
				assert.Empty(links)
			}
		})
	}
}

func TestMeetingRecordNotesAndCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"automatic notes", func(t *testing.T) {
			t.Helper()
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
			_, a := projectionFixture(t, st, "a", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			_, b := projectionFixture(t, st, "b", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "msg", MessageID: a}, {Kind: "msg", MessageID: b}} {
				record, err := st.MeetingRecordContext(t.Context(), ref)
				require.NoError(t, err)
				assert.Equal(t, []int64{a, b}, record.MessageIDs)
			}
		}},
		{"decisions and copies", func(t *testing.T) {
			t.Helper()
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "record@example.org"))
			_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", "record@example.org"), "second@example.com")
			_, automatic := projectionFixture(t, st, "automatic", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"record@example.org"}}`)
			_, manual := projectionFixture(t, st, "manual", "plaud_json", `{}`)
			_, excluded := projectionFixture(t, st, "excluded", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"record@example.org"}}`)
			require.NoError(st.LinkMeetingContext(t.Context(), manual, OccurrenceRef{MessageID: copyID}))
			require.NoError(st.UnlinkMeetingContext(t.Context(), excluded, OccurrenceRef{MessageID: copyID}))
			for _, id := range []int64{event, copyID} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
				require.NoError(err)
				assert.Equal([]int64{automatic, manual}, record.MessageIDs)
				require.Len(record.Links, 1)
				assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: id}), record.Links[0].Ref)
			}
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: automatic})
			require.NoError(err)
			assert.Equal([]int64{automatic, manual}, record.MessageIDs)
			require.Len(record.Links, 1)
			assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: event}), record.Links[0].Ref)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}
