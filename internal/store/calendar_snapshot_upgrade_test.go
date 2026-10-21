package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func snapshotAliasRaw(hour int) string {
	return fmt.Sprintf(`{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T%02d:00:00Z"},"end":{"dateTime":"2027-03-14T%02d:00:00Z"}}`, hour, hour+1)
}

func TestCalendarSnapshotBackfillRecognizesUTCEpoch(t *testing.T) {
	t.Setenv("PGTZ", "America/New_York")
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "instance", snapshotAliasRaw(12))
	estimate := time.Date(2004, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := st.db.Exec(`UPDATE messages SET last_modified = ? WHERE id = ?`, st.dialect.TimestampParam(estimate), id)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, id)
	requirements.NoError(err)
	// Leave freshness at the schema default, as the ADD COLUMN upgrade does.
	_, err = st.db.Exec(`INSERT INTO calendar_events (message_id, projection_version, content_hash, kind) VALUES (?, 1, 'legacy', 'single')`, id)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCalendarEventSnapshotFreshness)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	var stamp time.Time
	requirements.NoError(st.db.QueryRow(`SELECT snapshot_updated_at FROM calendar_events WHERE message_id = ?`, id).Scan(&stamp))
	assertions.Equal(estimate, stamp.UTC())
}

func TestCalendarSnapshotUpgradeFromSchema12(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "instance", snapshotAliasRaw(12))
	_, err := st.db.Exec(`DROP INDEX idx_calendar_events_exception_alias`)
	requirements.NoError(err)
	_, err = st.db.Exec(`ALTER TABLE calendar_events DROP COLUMN snapshot_updated_at`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE archive_metadata SET value = '12' WHERE key = 'schema_version'`)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCalendarEventSnapshotFreshness)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(id, page.Occurrences[0].MessageID)
	assertions.Equal(from.Add(12*time.Hour), page.Occurrences[0].Start)
}
