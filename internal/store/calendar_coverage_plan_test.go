package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestCalendarCoverageUsesIndexedHistoryLookups(t *testing.T) {
	requirements := require.New(t)
	st := openTestStore(t)
	// The first sync runs planner maintenance; all following runs stay inside
	// the six-hour throttle and leave the production statistics stale.
	optimizationTime := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	st.syncOptimizeNow = func() time.Time { return optimizationTime }
	completeSync := func(sourceID int64, syncType string) {
		syncID, err := st.StartSyncContext(t.Context(), sourceID, syncType)
		requirements.NoError(err)
		requirements.NoError(st.CompleteSyncAndUpdateSourceCursorContext(t.Context(), syncID, sourceID, "token"))
	}
	seed, err := st.GetOrCreateSource("gcal", "stats-seed@example.test")
	requirements.NoError(err)
	completeSync(seed.ID, "full")
	for range 29 {
		completeSync(seed.ID, "incremental")
	}
	ids := make([]int64, 0, 100)
	for i := range 100 {
		source, err := st.GetOrCreateSource("gcal", fmt.Sprintf("coverage-load-%03d@example.test", i))
		requirements.NoError(err)
		ids = append(ids, source.ID)
		completeSync(source.ID, "full")
	}
	_, err = st.db.ExecContext(t.Context(), `
		WITH RECURSIVE run_number(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM run_number WHERE n < 200
		)
		INSERT INTO sync_runs (source_id, sync_type, started_at, completed_at, status, cursor_after)
		SELECT s.id, 'incremental', datetime('now', printf('-%d minutes', run_number.n)),
			datetime('now', printf('-%d minutes', run_number.n)), 'completed', 'token'
		FROM sources s CROSS JOIN run_number
		WHERE s.source_type = 'gcal' AND s.identifier LIKE 'coverage-load-%'
	`)
	requirements.NoError(err)
	var runCount int
	requirements.NoError(st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sync_runs`).Scan(&runCount))
	requirements.Equal(20_130, runCount)
	ctx := sourcescope.WithSourceIDs(t.Context(), ids)
	query, args := calendarCoverageQuery(ctx, st.IsPostgreSQL())
	plans, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+st.Rebind(query), args...)
	requirements.NoError(err)
	var details []string
	for plans.Next() {
		var id, parent, unused int
		var detail string
		requirements.NoError(plans.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requirements.NoError(plans.Err())
	requirements.NoError(plans.Close())
	var indexedRunLookups int
	for _, detail := range details {
		if strings.Contains(detail, "idx_sync_runs_source") {
			indexedRunLookups++
		}
	}
	plan := strings.Join(details, "; ")
	for _, alias := range []string{"r", "latest", "failure"} {
		requirements.NotContains(plan, "SCAN "+alias, "coverage should not scan sync history; plan: %s", plan)
	}
	requirements.GreaterOrEqual(indexedRunLookups, 3, "coverage should use indexed lookups for full and latest baselines plus failure checks; plan: %s", plan)
}
