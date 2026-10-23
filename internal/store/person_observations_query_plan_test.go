package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type observationSupportQueryRecorder struct {
	contextRowsQuerier
	query string
	args  []any
}

func (q *observationSupportQueryRecorder) QueryContext(ctx context.Context, query string, args ...any) (*loggedRows, error) {
	q.query = query
	q.args = append([]any(nil), args...)
	return q.contextRowsQuerier.QueryContext(ctx, query, args...)
}

// Explain the production lookup itself: without the fact ledger's scoped
// index, every observation citation scans the archive-wide status history.
func TestObservationSupportLookupUsesEvidenceStatusIndex(t *testing.T) {
	st := openTestStore(t) // EXPLAIN QUERY PLAN is SQLite-specific.
	recorder := &observationSupportQueryRecorder{contextRowsQuerier: st.db}
	_, err := st.latestEvidenceSupportedTx(t.Context(), recorder, []int64{1})
	require.NoError(t, err)
	require.NotEmpty(t, recorder.query)
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+recorder.query, recorder.args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var steps []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		steps = append(steps, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(steps, "\n")
	assert.Contains(t, plan, "SEARCH latest USING INDEX idx_person_fact_evidence_status_latest", plan)
	assert.NotContains(t, plan, "SCAN latest", plan)
}
