package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This catches a lane status query that scans every historical run and sorts
// it in a temporary b-tree. sync_runs is never pruned, so the active and
// latest-successful lookups must walk a status-filtered index in run order.
func TestOperationLaneStatusQueriesUseStatusIndexesSQLite(t *testing.T) {
	st, err := OpenForTest(filepath.Join(t.TempDir(), "operation-status-plan.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())
	collation := st.bytewiseTextCollation()

	tests := []struct {
		name      string
		query     string
		wantIndex string
	}{
		{
			name:      "active source run",
			query:     sourceOperationStatusQuery(`status = 'running'`),
			wantIndex: "idx_sync_runs_operations_running",
		},
		{
			name:      "latest successful source run",
			query:     sourceOperationStatusQuery(`status = 'completed' AND errors_count = 0`),
			wantIndex: "idx_sync_runs_operations_succeeded",
		},
		{
			name: "active person sweep run",
			query: personSweepOperationStatusQuery(
				personSweepOperationRunColumns(collation), collation, `r.status = 'running'`),
			wantIndex: "idx_person_sweep_runs_operations_running",
		},
		{
			name: "latest successful person sweep run",
			query: personSweepOperationStatusQuery(
				personSweepOperationRunColumns(collation), collation, `r.status = 'succeeded'`),
			wantIndex: "idx_person_sweep_runs_operations_succeeded",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+test.query)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			details := make([]string, 0)
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(rows.Err())
			plan := strings.Join(details, "\n")
			assert.NotContains(plan, "USE TEMP B-TREE")
			assert.Contains(plan, "USING INDEX "+test.wantIndex)
		})
	}
}
