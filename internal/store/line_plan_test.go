package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineInboxStatusUsesReceivedIndexSQLite(t *testing.T) {
	st, err := OpenForTest(filepath.Join(t.TempDir(), "line-status-plan.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT MAX(received_at) FROM line_webhook_events WHERE source_id=?", 1)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, strings.Join(details, "\n"), "USING COVERING INDEX idx_line_webhook_received (source_id=?)")
}
