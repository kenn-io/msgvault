package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarEventDurationUpgrade(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "duration-upgrade", calendarRaw)
	for _, query := range []string{
		`ALTER TABLE calendar_events DROP COLUMN recurrence_duration_days`,
		`DELETE FROM applied_migrations WHERE name = 'calendar_event_recurrence_duration_v1'`,
		`UPDATE archive_metadata SET value = '11' WHERE key = 'schema_version'`,
	} {
		_, err := st.db.Exec(query)
		requirements.NoError(err)
	}
	for range 2 {
		requirements.NoError(st.InitSchemaContext(t.Context()))
		rows := calendarRows(t, st, id)
		requirements.Len(rows, 1)
		assertions.Equal("event-example", rows[0].Event.ProviderEventID)
		assertions.Zero(rows[0].Event.RecurrenceDurationDays)
	}
}
