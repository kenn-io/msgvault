package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityCancelledCalendarUnreadableRawStaysIneligible(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := calendarFixture(t, st, "cancelled-unreadable", calendarRaw)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 2, 17, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"status":"cancelled"}`, Valid: true}))
	before, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
	requirements.NoError(err)
	requirements.Len(before, 1)
	requirements.NotNil(before[0].SentAt)
	requirements.False(before[0].Eligible)

	// Replacing the retained raw evidence refreshes the projection to a failure marker.
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"id":`), "gcal_json"))
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	requirements.Equal(calendarEventDecodeFailureStatus, rows[0].Event.Status)
	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.Equal(before[0].SentAt, candidates[0].SentAt)
	assertions.False(candidates[0].Eligible, "a decode failure must preserve the metadata cancellation")
}

func TestActivityAllDayICSDamagedMetadataKeepsDateOnly(t *testing.T) {
	for _, tc := range []struct{ name, metadata string }{
		{"null", ""},
		{"unreadable", `{"ics_version":"invalid"}`},
		{"conflicting", `{"ics_version":2,"all_day":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			imported, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART;VALUE=DATE:20270302\r\nDTEND;VALUE=DATE:20270303"))
			requirements.NoError(err)
			requirements.Len(imported.MessageIDs, 1)
			id := imported.MessageIDs[0]
			before := calendarRows(t, st, id)
			requirements.Len(before, 1)
			requirements.True(before[0].Event.AllDay)
			_, err = st.db.Exec(`UPDATE messages SET metadata=`+st.dialect.JSONBindExpr()+` WHERE id=?`, sql.NullString{String: tc.metadata, Valid: tc.metadata != ""}, id)
			requirements.NoError(err)
			// A failed rebuild keeps the accepted all-day projection.
			requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
				return st.refreshCalendarEventProjectionWith(t.Context(), tx, id)
			}))
			assertions.Equal(before[0].Event, calendarRows(t, st, id)[0].Event)
			candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			requirements.NoError(err)
			requirements.Len(candidates, 1)
			requirements.NotNil(candidates[0].SentAt)
			assertions.Equal("2027-03-02T00:00:00Z", candidates[0].SentAt.Format(time.RFC3339))
			assertions.True(candidates[0].Eligible)
			assertions.True(candidates[0].DateOnly, "an accepted all-day event must keep its calendar date across time zones")
		})
	}
}
