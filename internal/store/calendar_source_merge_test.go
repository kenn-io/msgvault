package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestListCalendarOccurrencesSourceMergeArchiveOnly(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	addEvent := func(sourceID int64, providerID, raw string) int64 {
		id, _ := mergeFixtureMessage(t, st, sourceID, providerID, providerID, fmt.Sprintf("calendar source %d", sourceID), 0)
		_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET message_type = 'calendar_event' WHERE id = ?`), id)
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(raw), "gcal_json"))
		return id
	}
	from, err := st.GetOrCreateSource("gcal", "history@example.test")
	requirements.NoError(err)
	into, err := st.GetOrCreateSource("gcal", "live@example.test")
	requirements.NoError(err)
	masterRaw := `{"id":"weekly","status":"confirmed","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
	addEvent(from.ID, "weekly", masterRaw)
	master := addEvent(into.ID, "weekly", masterRaw)
	for _, source := range []int64{into.ID, from.ID} {
		cancelled, moved := "confirmed", "2027-03-16T09:00:00Z"
		if source == from.ID {
			cancelled, moved = "cancelled", "2027-04-02T09:00:00Z"
		}
		addEvent(source, "cancel", `{"id":"cancel","recurringEventId":"weekly","status":"`+cancelled+`","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"}}`)
		addEvent(source, "move", `{"id":"move","recurringEventId":"weekly","status":"confirmed","originalStartTime":{"dateTime":"2027-03-16T09:00:00Z"},"start":{"dateTime":"`+moved+`"},"end":{"dateTime":"`+moved[:11]+`10:00:00Z"}}`)
	}
	window := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	before, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 2, 0))
	requirements.NoError(err)
	var want []store.CalendarOccurrence
	for _, occurrence := range before.Occurrences {
		if occurrence.SourceID == into.ID {
			want = append(want, occurrence)
		}
	}
	requirements.Len(want, 3)
	assertions.Equal(master, want[0].MessageID)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	requirements.NoError(err)
	var archiveOnly int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM source_merge_archive_only_messages`).Scan(&archiveOnly))
	requirements.Equal(3, archiveOnly)
	after, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 2, 0))
	requirements.NoError(err)
	assertions.Empty(after.Unexpandable)
	assertions.Equal(want, after.Occurrences)
}

func calendarMergeEvent(t *testing.T, st *store.Store, sourceID int64, providerID, raw string) int64 {
	t.Helper()
	requirements := require.New(t)
	id, _ := mergeFixtureMessage(t, st, sourceID, providerID, providerID, fmt.Sprintf("calendar source %d", sourceID), 0)
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET message_type = 'calendar_event' WHERE id = ?`), id)
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(raw), "gcal_json"))
	return id
}

func TestCalendarSourceMergeUniqueEvent(t *testing.T) {
	for _, destination := range []string{"empty", "non-overlapping", "deleted duplicate"} {
		t.Run(destination, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("gcal", "history@example.test")
			requirements.NoError(err)
			into, err := st.GetOrCreateSource("gcal", "live@example.test")
			requirements.NoError(err)
			raw := `{"id":"unique","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"}}`
			event := calendarMergeEvent(t, st, from.ID, "unique", raw)
			if destination == "non-overlapping" {
				calendarMergeEvent(t, st, into.ID, "other", `{"id":"other","start":{"dateTime":"2027-04-02T09:00:00Z"},"end":{"dateTime":"2027-04-02T10:00:00Z"}}`)
			}
			if destination == "deleted duplicate" {
				deleted := calendarMergeEvent(t, st, into.ID, "unique", raw)
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), time.Now(), deleted)
				requirements.NoError(err)
			}
			window := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			before, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 1, 0))
			requirements.NoError(err)
			requirements.Len(before.Occurrences, 1)
			assertions.Equal(event, before.Occurrences[0].MessageID)
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			requirements.NoError(err)
			after, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 1, 0))
			requirements.NoError(err)
			assertions.Empty(after.Unexpandable)
			requirements.Len(after.Occurrences, 1)
			assertions.Equal(event, after.Occurrences[0].MessageID)
			assertions.Equal(into.ID, after.Occurrences[0].SourceID)
		})
	}
}

func TestCalendarSourceMergeSplitSeries(t *testing.T) {
	for _, incoming := range []string{"exception", "master"} {
		t.Run(incoming, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("gcal", "history@example.test")
			requirements.NoError(err)
			into, err := st.GetOrCreateSource("gcal", "live@example.test")
			requirements.NoError(err)
			masterSource, exceptionSource := into.ID, from.ID
			if incoming == "master" {
				masterSource, exceptionSource = from.ID, into.ID
			}
			master := calendarMergeEvent(t, st, masterSource, "weekly", `{"id":"weekly","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
			movedRaw := `{"id":"move","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"},"start":{"dateTime":"2027-03-10T09:00:00Z"},"end":{"dateTime":"2027-03-10T10:00:00Z"}}`
			moved := calendarMergeEvent(t, st, exceptionSource, "move", movedRaw)
			if incoming == "master" {
				calendarMergeEvent(t, st, from.ID, "move", movedRaw)
			}
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			requirements.NoError(err)
			window := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 1, 0))
			requirements.NoError(err)
			assertions.Empty(page.Unexpandable)
			requirements.Len(page.Occurrences, 3)
			assertions.Equal(master, page.Occurrences[0].MessageID)
			assertions.Equal(moved, page.Occurrences[1].MessageID)
			assertions.Equal(master, page.Occurrences[1].SeriesMessageID)
			assertions.Equal("2027-03-10T09:00:00Z", page.Occurrences[1].Start.Format(time.RFC3339))
			assertions.Equal(master, page.Occurrences[2].MessageID)
		})
	}
}

func TestCalendarSourceMergeOverlappingIncomingSources(t *testing.T) {
	for _, order := range []string{"oldest first", "newest first"} {
		t.Run(order, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			first, err := st.GetOrCreateSource("gcal", "first@example.test")
			requirements.NoError(err)
			second, err := st.GetOrCreateSource("gcal", "second@example.test")
			requirements.NoError(err)
			into, err := st.GetOrCreateSource("gcal", "live@example.test")
			requirements.NoError(err)
			raw := `{"id":"shared","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"}}`
			oldest := calendarMergeEvent(t, st, first.ID, "shared", raw)
			calendarMergeEvent(t, st, second.ID, "shared", raw)
			sources := []int64{first.ID, second.ID}
			if order == "newest first" {
				sources = []int64{second.ID, first.ID}
			}
			for _, source := range sources {
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source, IntoSourceID: into.ID})
				requirements.NoError(err)
			}
			window := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 1, 0))
			requirements.NoError(err)
			assertions.Empty(page.Unexpandable)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(oldest, page.Occurrences[0].MessageID)
			assertions.Equal(into.ID, page.Occurrences[0].SourceID)
		})
	}
}

func TestCalendarSourceMergeArchiveOnlyMasterLookup(t *testing.T) {
	for _, bounds := range []struct{ name, start, end, original, wantEnd string }{
		{"timed", `{"dateTime":"2027-03-02T09:00:00Z"}`, `{"dateTime":"2027-03-05T09:00:00Z"}`, `{"dateTime":"2027-03-09T09:00:00Z"}`, "2027-03-12T09:00:00Z"},
		{"all-day", `{"date":"2027-03-02"}`, `{"date":"2027-03-06"}`, `{"date":"2027-03-09"}`, "2027-03-13T00:00:00Z"},
	} {
		t.Run(bounds.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("gcal", "history@example.test")
			requirements.NoError(err)
			into, err := st.GetOrCreateSource("gcal", "live@example.test")
			requirements.NoError(err)
			master := calendarMergeEvent(t, st, from.ID, "weekly", fmt.Sprintf(`{"id":"weekly","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=1"]}`, bounds.start, bounds.end))
			cancelled := calendarMergeEvent(t, st, into.ID, "cancelled", fmt.Sprintf(`{"id":"cancelled","recurringEventId":"weekly","originalStartTime":%s,"status":"cancelled"}`, bounds.original))
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			requirements.NoError(err)
			// The original start and the shortened master are outside the candidate
			// margin. Only the master lookup can recover this cancellation's end.
			window := time.Date(2027, 3, 12, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 0, 1))
			requirements.NoError(err)
			assertions.Empty(page.Unexpandable)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(cancelled, page.Occurrences[0].MessageID)
			assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
			assertions.Equal("cancelled", page.Occurrences[0].Status)
			assertions.Equal(bounds.wantEnd, page.Occurrences[0].End.Format(time.RFC3339))
		})
	}
}
