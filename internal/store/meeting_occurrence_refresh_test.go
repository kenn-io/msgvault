package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingFallbackReader struct {
	SQLReader
	t    *testing.T
	rows int
}

func (r *countingFallbackReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "calendar_event_attendees") && strings.Contains(query, "rsvp='declined'") {
		var n int
		require.NoError(r.t, r.SQLReader.QueryRowContext(ctx, "SELECT count(*) FROM ("+query+")", args...).Scan(&n))
		r.rows += n
	}
	return r.SQLReader.QueryContext(ctx, query, args...)
}

func TestMeetingRefreshSkipsCancelledFuture(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, _ = calendarFixture(t, st, "cancelled", `{"id":"cancelled","status":"cancelled","start":{"dateTime":"2027-03-10T10:00:00Z"},"end":{"dateTime":"2027-03-10T11:00:00Z"}}`)
	_, _ = calendarFixture(t, st, "live", `{"id":"live","start":{"dateTime":"2027-04-01T10:00:00Z"},"end":{"dateTime":"2027-04-01T11:00:00Z"}}`)
	_, next, err := st.MeetingOccurrencesContext(t.Context(), time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	assert.Equal(t, time.Date(2027, 4, 1, 10, 0, 0, 0, time.UTC), next)
}

func TestMeetingFallbackReadsOnlyFallbackAttendees(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "unsupported", `{"id":"unsupported","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"],"attendees":[{"email":"calendar@example.com","responseStatus":"declined"}]}`)
	_, _ = calendarFixture(t, st, "ordinary", `{"id":"ordinary","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"attendees":[{"email":"someone@example.com","responseStatus":"declined"}]}`)
	reader := &countingFallbackReader{SQLReader: st.meetingSQLReader(t.Context()), t: t}
	ctx := context.WithValue(t.Context(), meetingSQLReaderKey{}, reader)
	evidence, err := st.loadMeetingEvidence(ctx, time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	rows, _, err := evidence.Reduce(t.Context(), map[string]bool{"calendar@example.com": true})
	require.NoError(t, err)
	require.Contains(t, evidence.fallbacks, id)
	assert.Equal(t, []string{"calendar@example.com"}, evidence.fallbacks[id].declined)
	assert.Equal(t, 1, reader.rows)
	found := false
	for _, row := range rows {
		if row.MessageID == id {
			found = true
			assert.False(t, row.Counted)
		}
	}
	assert.True(t, found)
}
