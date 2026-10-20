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
