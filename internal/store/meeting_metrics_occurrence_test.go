package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestMeetingOccurrenceEvidence(t *testing.T) {
	through := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	t.Run("excluded-series", func(t *testing.T) {
		st := newRFC822IDBackfillBackendStore(t)
		_, event := calendarFixture(t, st, "series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=1","EXDATE:20270314T100000Z"]}`)
		rows, _, err := st.MeetingOccurrencesContext(t.Context(), through, nil)
		require.NoError(t, err)
		var found bool
		for _, row := range rows {
			if row.MessageID == event {
				found = true
				assert.False(t, row.Counted)
			}
		}
		assert.True(t, found, "a supported series must suppress message fallback")
	})
	t.Run("future-note-refresh", func(t *testing.T) {
		st := newRFC822IDBackfillBackendStore(t)
		data, _ := projectionFixture(t, st, "future", "meeting_json", `{"transcript":"Notes"}`)
		start := through.Add(time.Hour)
		data.Message.SentAt = sql.NullTime{Time: start, Valid: true}
		_, err := st.PersistMessage(data)
		require.NoError(t, err)
		_, next, err := st.MeetingOccurrencesContext(t.Context(), through, nil)
		require.NoError(t, err)
		assert.True(t, start.Equal(next))
	})
	t.Run("future-unsupported-refresh", func(t *testing.T) {
		st := newRFC822IDBackfillBackendStore(t)
		_, _ = calendarFixture(t, st, "future", `{"id":"future","start":{"dateTime":"2027-04-01T01:00:00Z"},"end":{"dateTime":"2027-04-01T02:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
		_, next, err := st.MeetingOccurrencesContext(t.Context(), through, nil)
		require.NoError(t, err)
		assert.True(t, through.Add(time.Hour).Equal(next))
	})
	t.Run("dated-linked-notes", func(t *testing.T) {
		st := newRFC822IDBackfillBackendStore(t)
		_, a := projectionFixture(t, st, "undated", "meeting_json", `{"transcript":"Notes"}`)
		data, b := projectionFixture(t, st, "dated", "meeting_json", `{"transcript":"Notes"}`)
		at := through.Add(-time.Hour)
		data.Message.SentAt = sql.NullTime{Time: at, Valid: true}
		_, err := st.PersistMessage(data)
		require.NoError(t, err)
		_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: a}, MeetingLinkRef{Kind: "msg", MessageID: b}, "link")
		require.NoError(t, err)
		rows, _, err := st.MeetingOccurrencesContext(t.Context(), through, nil)
		require.NoError(t, err)
		for _, row := range rows {
			if row.MessageID == a || row.MessageID == b {
				assert.True(t, at.Equal(row.OccurredAt))
			}
		}
	})
	t.Run("removed-attendee", func(t *testing.T) {
		st := newRFC822IDBackfillBackendStore(t)
		_, master := calendarFixture(t, st, "series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"person@example.com","responseStatus":"accepted"}],"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
		_, _ = calendarFixture(t, st, "edited", `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"start":{"dateTime":"2027-03-21T10:00:00Z"},"end":{"dateTime":"2027-03-21T11:00:00Z"},"attendees":[]}`)
		rows, _, err := st.MeetingOccurrencesContext(t.Context(), through, nil)
		require.NoError(t, err)
		var found bool
		for _, row := range rows {
			if row.MessageID == master && row.OccurredAt.Day() == 21 {
				found = true
				assert.Contains(t, row.DeclinedEmails, "person@example.com")
			}
		}
		assert.True(t, found)
	})
}

func TestMeetingMetricsLinkedNotes(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	eventData, event := calendarFixture(t, st, "event", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
	_, first := projectionFixture(t, st, "first", "meeting_json", `{"transcript":"Notes","started_at":"2024-01-02T10:00:00Z","ended_at":"2024-01-02T10:30:00Z"}`)
	_, second := projectionFixture(t, st, "second", "meeting_json", `{"transcript":"Notes"}`)
	for _, note := range []int64{first, second} {
		_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
		require.NoError(t, err)
	}
	ids := []int64{first, second}
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	require.NoError(t, err)
	assert.EqualValues(t, 1, metrics.Totals.MeetingCount)
	assert.InDelta(t, 1800, metrics.Totals.TotalKnownSeconds, 0)
	require.Len(t, metrics.DurationByBasis, 1)
	assert.Equal(t, meetingcontent.DurationProvider, metrics.DurationByBasis[0].Basis)
	calendarMetrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{SourceIDs: []int64{eventData.Message.SourceID}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, calendarMetrics.Totals.MeetingCount)
	assert.InDelta(t, 3600, calendarMetrics.Totals.TotalKnownSeconds, 0)
	allMetrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, allMetrics.Totals.MeetingCount)
	assert.InDelta(t, 1800, allMetrics.Totals.TotalKnownSeconds, 0)
}

func TestMeetingMetricsUnsupportedFutureEvent(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	raw := `{"id":"future","start":{"dateTime":"` + start.Format(time.RFC3339) + `"},"end":{"dateTime":"` + start.Add(time.Hour).Format(time.RFC3339) + `"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`
	_, _ = calendarFixture(t, st, "future-metrics", raw)
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{})
	require.NoError(t, err)
	assert.Zero(t, metrics.Totals.MeetingCount)
}

func TestMeetingMetricsIgnoresPendingDuration(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "duration-event", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "pending-duration", "meeting_json", `{"started_at":"2024-01-02T10:00:00Z","ended_at":"2024-01-02T10:30:00Z"}`)
	_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
	require.NoError(t, err)
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, metrics.Totals.MeetingCount)
	assert.InDelta(t, 3600, metrics.Totals.TotalKnownSeconds, 0)
}

func TestMeetingMetricsEditedOccurrenceDuration(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, _ = calendarFixture(t, st, "duration-series", `{"id":"series","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, _ = calendarFixture(t, st, "duration-edit", `{"id":"moved","recurringEventId":"series","originalStartTime":{"dateTime":"2024-01-09T10:00:00Z"},"start":{"dateTime":"2024-01-09T12:00:00Z"},"end":{"dateTime":"2024-01-09T14:00:00Z"}}`)
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{})
	require.NoError(t, err)
	assert.EqualValues(t, 2, metrics.Totals.MeetingCount)
	assert.InDelta(t, 10800, metrics.Totals.TotalKnownSeconds, 0)
	for _, tc := range []struct {
		after, before time.Time
		seconds       float64
	}{
		{time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC), 3600},
		{time.Date(2024, 1, 9, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC), 7200},
	} {
		metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{After: &tc.after, Before: &tc.before})
		require.NoError(t, err)
		assert.EqualValues(t, 1, metrics.Totals.MeetingCount)
		assert.InDelta(t, tc.seconds, metrics.Totals.TotalKnownSeconds, 0)
	}
}

func TestMeetingMetricsScopedCalendarCopies(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for _, tc := range []struct{ owner, start, end string }{
		{"first@example.com", "2024-01-02T10:00:00Z", "2024-01-02T11:00:00Z"},
		{"second@example.com", "2024-01-03T12:00:00Z", "2024-01-03T14:00:00Z"},
	} {
		_, _ = calendarFixture(t, st, "master-"+tc.owner, `{"id":"series","iCalUID":"shared@example.org","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=1"]}`, tc.owner)
		data, _ := calendarFixture(t, st, "edit-"+tc.owner, fmt.Sprintf(`{"id":"edit","recurringEventId":"series","iCalUID":"shared@example.org","originalStartTime":{"dateTime":"2024-01-02T10:00:00Z"},"start":{"dateTime":%q},"end":{"dateTime":%q}}`, tc.start, tc.end), tc.owner)
		metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{SourceIDs: []int64{data.Message.SourceID}})
		require.NoError(t, err)
		require.EqualValues(t, 1, metrics.Totals.MeetingCount)
		start, err := time.Parse(time.RFC3339, tc.start)
		require.NoError(t, err)
		end, err := time.Parse(time.RFC3339, tc.end)
		require.NoError(t, err)
		assert.InDelta(t, end.Sub(start).Seconds(), metrics.Totals.TotalKnownSeconds, 0)
		require.NotNil(t, metrics.FirstMeetingAt)
		assert.True(t, start.Equal(*metrics.FirstMeetingAt))
	}
}

func TestMeetingMetricsSelectedNoteUsesEventDuration(t *testing.T) {
	for _, raw := range []string{`{"transcript":"Notes"}`, `{}`} {
		t.Run(raw, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			eventData, event := calendarFixture(t, st, "selected-event", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
			noteData, note := projectionFixture(t, st, "selected-note", "meeting_json", raw)
			_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
			require.NoError(t, err)
			ids := []int64{note}
			metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
			require.NoError(t, err)
			assert.EqualValues(t, 1, metrics.Totals.MeetingCount)
			assert.InDelta(t, 3600, metrics.Totals.TotalKnownSeconds, 0)
			calendarScope, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids, SourceIDs: []int64{eventData.Message.SourceID}})
			require.NoError(t, err)
			assert.EqualValues(t, 1, calendarScope.Totals.MeetingCount)
			assert.InDelta(t, 3600, calendarScope.Totals.TotalKnownSeconds, 0)
			noteScope, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids, SourceIDs: []int64{noteData.Message.SourceID}})
			require.NoError(t, err)
			if raw == `{}` {
				assert.Zero(t, noteScope.Totals.MeetingCount)
			} else {
				assert.EqualValues(t, 1, noteScope.Totals.MeetingCount)
				assert.EqualValues(t, 1, noteScope.Totals.UnknownDurationCount)
			}
			authorized := sourcescope.WithSourceIDs(t.Context(), []int64{noteData.Message.SourceID})
			limited, err := st.GetMeetingMetricsContext(authorized, MeetingQueryScope{MessageIDs: &ids})
			require.NoError(t, err)
			assert.Equal(t, noteScope.Totals, limited.Totals)
		})
	}
}

func TestMeetingOccurrenceNoteOnlyMatching(t *testing.T) {
	for _, tc := range []struct {
		name, format, raw string
		series            bool
		want              int
	}{
		{"google-no-calendar", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series"},"transcript":[{"text":"Notes"}]}`, false, 2},
		{"google-series", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series"},"transcript":[{"text":"Notes"}]}`, true, 4},
		{"uid-and-time", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Notes"}]}]},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2024-01-02T10:00:00Z"}}`, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			if tc.series {
				_, _ = calendarFixture(t, st, "series", `{"id":"series","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
			}
			_, first := projectionFixture(t, st, "first-note", tc.format, tc.raw)
			_, second := projectionFixture(t, st, "second-note", tc.format, tc.raw)
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: first})
			require.NoError(t, err)
			if tc.format == "granola_json" {
				assert.Equal(t, []int64{first}, record.MessageIDs)
				assert.Empty(t, record.Links)
			} else {
				assert.ElementsMatch(t, []int64{first, second}, record.MessageIDs)
			}
			metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{})
			require.NoError(t, err)
			assert.EqualValues(t, tc.want, metrics.Totals.MeetingCount)
		})
	}
}

func TestMeetingMetricsExcludeDeclinedPerson(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "declined", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"attendees":[{"email":"person@example.com","responseStatus":"declined"}]}`)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	require.NoError(t, st.ReplaceMessageRecipients(event, "to", []int64{person}, []string{""}))
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{ParticipantIDs: []int64{person}})
	require.NoError(t, err)
	assert.Zero(t, metrics.Totals.MeetingCount)
	personScope, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{Person: &personscope.Scope{ParticipantIDs: []int64{person}, Directions: []personscope.Direction{personscope.ToPerson}}})
	require.NoError(t, err)
	assert.Zero(t, personScope.Totals.MeetingCount)
	domainScope, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{Domains: []string{"example.com"}})
	require.NoError(t, err)
	assert.Zero(t, domainScope.Totals.MeetingCount)
	alias, err := st.EnsureParticipant("alias@example.com", "Alias", "example.com")
	require.NoError(t, err)
	_, err = st.LinkParticipants(person, alias)
	require.NoError(t, err)
	accepted, err := st.EnsureParticipant("accepted@example.com", "Accepted", "example.com")
	require.NoError(t, err)
	require.NoError(t, st.ReplaceMessageRecipients(event, "to", []int64{person, alias, accepted}, []string{"", "", ""}))
	aliasScope, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{ParticipantIDs: []int64{alias}})
	require.NoError(t, err)
	assert.Zero(t, aliasScope.Totals.MeetingCount)
	mixedScope, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{ParticipantIDs: []int64{person, accepted}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, mixedScope.Totals.MeetingCount)
	domainScope, err = st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{Domains: []string{"example.com"}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, domainScope.Totals.MeetingCount)
}

func TestMeetingMetricsEditedOccurrenceWithOmittedAttendees(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", `{"id":"series","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"attendees":[{"email":"person@example.com","responseStatus":"accepted"}],"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	edit, _ := calendarFixture(t, st, "edited", `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2024-01-09T10:00:00Z"},"start":{"dateTime":"2024-01-09T10:00:00Z"},"end":{"dateTime":"2024-01-09T11:00:00Z"},"attendeesOmitted":true}`)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	require.NoError(t, st.ReplaceMessageRecipients(master, "to", []int64{person}, []string{""}))
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{ParticipantIDs: []int64{person}})
	require.NoError(t, err)
	assert.EqualValues(t, 2, metrics.Totals.MeetingCount)
	var before int64
	if !st.IsPostgreSQL() {
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&before))
	}
	edit.RawMIME = []byte(`{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2024-01-09T10:00:00Z"},"start":{"dateTime":"2024-01-09T10:00:00Z"},"end":{"dateTime":"2024-01-09T11:00:00Z"},"attendeesOmitted":false}`)
	_, err = st.PersistMessage(edit)
	require.NoError(t, err)
	metrics, err = st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{ParticipantIDs: []int64{person}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, metrics.Totals.MeetingCount)
	if !st.IsPostgreSQL() {
		var changes int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset='meeting_occurrences'`, before).Scan(&changes))
		assert.Positive(t, changes)
	}
	edit.RawMIME = []byte(`{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2024-01-09T10:00:00Z"},"start":{"dateTime":"2024-01-09T10:00:00Z"},"end":{"dateTime":"2024-01-09T11:00:00Z"},"attendeesOmitted":true,"attendees":[{"email":"person@example.com","responseStatus":"declined"}]}`)
	_, err = st.PersistMessage(edit)
	require.NoError(t, err)
	metrics, err = st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{ParticipantIDs: []int64{person}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, metrics.Totals.MeetingCount)
}

func TestMeetingOccurrenceFutureLinkedNote(t *testing.T) {
	for _, tc := range []struct {
		name      string
		start     time.Time
		automatic bool
	}{
		{"same-year", time.Date(2027, 4, 1, 1, 0, 0, 0, time.UTC), false},
		{"later-year-choice", time.Date(2029, 4, 1, 1, 0, 0, 0, time.UTC), false},
		{"later-year-automatic", time.Date(2029, 4, 1, 1, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "future-linked", fmt.Sprintf(`{"id":"future","start":{"dateTime":%q},"end":{"dateTime":%q}}`, tc.start.Format(time.RFC3339), tc.start.Add(time.Hour).Format(time.RFC3339)))
			format, raw := "meeting_json", `{"transcript":"Notes"}`
			if tc.automatic {
				format, raw = "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"future"},"transcript":[{"text":"Notes"}]}`
			}
			data, note := projectionFixture(t, st, "old-note", format, raw)
			data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), Valid: true}
			_, err := st.PersistMessage(data)
			require.NoError(t, err)
			if !tc.automatic {
				_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
				require.NoError(t, err)
			}
			rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), nil)
			require.NoError(t, err)
			found := false
			for _, row := range rows {
				if row.MessageID == note {
					found = true
					assert.False(t, row.Counted && !row.OccurredAt.After(time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)))
				}
			}
			assert.True(t, found)
			rows, _, err = st.MeetingOccurrencesContext(t.Context(), tc.start.Add(time.Second), nil)
			require.NoError(t, err)
			found = false
			for _, row := range rows {
				if row.MessageID == note && row.Counted {
					found = true
					assert.True(t, tc.start.Equal(row.OccurredAt))
				}
			}
			assert.True(t, found)
		})
	}
}

func TestMeetingOccurrenceChoiceSurvivesDedup(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, survivor := projectionFixture(t, st, "survivor", "meeting_json", `{"transcript":"Notes"}`)
	_, duplicate := projectionFixture(t, st, "duplicate", "meeting_json", `{"transcript":"Notes"}`)
	_, other := projectionFixture(t, st, "other", "meeting_json", `{"transcript":"Notes"}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
	require.NoError(t, err)
	_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "counts-dedup")
	require.NoError(t, err)
	rows, _, err := st.MeetingOccurrencesContext(t.Context(), time.Now().UTC(), nil)
	require.NoError(t, err)
	keys := map[int64]string{}
	for _, row := range rows {
		if row.Counted {
			keys[row.MessageID] = row.OccurrenceKey
		}
	}
	assert.NotEmpty(t, keys[survivor])
	assert.Equal(t, keys[survivor], keys[other])
}

func TestMeetingMetricsNoteLinkedToTwoDates(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "two-dates", `{"id":"series","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, note := projectionFixture(t, st, "two-dates-note", "meeting_json", `{"transcript":"Notes"}`)
	for _, date := range []string{"20240102T100000Z", "20240109T100000Z"} {
		_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, date)}, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
		require.NoError(t, err)
	}
	ids := []int64{note}
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	require.NoError(t, err)
	assert.EqualValues(t, 2, metrics.Totals.MeetingCount)
	assert.EqualValues(t, 2, metrics.Totals.KnownDurationCount)
	assert.InDelta(t, 7200, metrics.Totals.TotalKnownSeconds, 0)
}

func TestMeetingMetricsAllDayUsesCivilMonth(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "all-day", `{"id":"all-day","start":{"date":"2024-03-01"},"end":{"date":"2024-03-02"}}`)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"Pacific/Kiritimati"}`))
	metrics, err := st.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{})
	require.NoError(t, err)
	require.Len(t, metrics.Months, 1)
	assert.Equal(t, "2024-03", metrics.Months[0].Month)
	require.NotNil(t, metrics.FirstMeetingAt)
	assert.Equal(t, "2024-02-29", metrics.FirstMeetingAt.UTC().Format(time.DateOnly))
}
