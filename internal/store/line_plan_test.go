package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineInboxStatusUsesReceivedIndexSQLite(t *testing.T) {
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "line-status-plan.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT MAX(received_at) FROM line_webhook_events WHERE source_id=?", 1)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(rows.Err())
	assert.Contains(t, strings.Join(details, "\n"), "USING COVERING INDEX idx_line_webhook_received (source_id=?)")
}
