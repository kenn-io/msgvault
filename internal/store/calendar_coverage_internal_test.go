package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarCoverageFreshnessBoundary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := openTestStore(t)
	src, err := st.GetOrCreateSource("gcal", "boundary@example.com")
	requirements.NoError(err)
	at := time.Date(2027, 3, 1, 10, 0, 0, 0, time.FixedZone("fixture", 3600))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor='token' WHERE id=?`), src.ID)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_runs(source_id,sync_type,started_at,completed_at,status,cursor_after) VALUES (?,'full',?,?,'completed','token')`), src.ID, at.Add(-time.Minute), at)
	requirements.NoError(err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	for _, tc := range []struct {
		elapsed time.Duration
		want    string
	}{{24 * time.Hour, "complete"}, {24*time.Hour + time.Nanosecond, "stale"}} {
		coverage, err := calendarCoverageContext(ctx, st, ReadDBContext(ctx, st.db.DB), nil, false, at.Add(tc.elapsed))
		requirements.NoError(err)
		requirements.Len(coverage.Sources, 1)
		assertions.Equal(tc.want, coverage.Sources[0].Status)
		requirements.NotNil(coverage.Sources[0].AsOf)
		assertions.Equal(at.UTC(), *coverage.Sources[0].AsOf)
	}
}
