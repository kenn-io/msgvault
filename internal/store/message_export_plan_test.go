package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A source-filtered export must seek conversations by source instead of
// scanning every conversation in the archive.
func TestMessageExportConversationsQuerySeeksBySourceSQLite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "message-export-plan.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	query, args := messageExportConversationsQuery(MessageExportFilter{
		Start: start, End: start.Add(time.Hour),
		SourceIDs: []int64{1}, MessageTypes: []string{"wanted"},
	})
	plan := explainPlan(t, st, query, args...)
	assert.Contains(plan, "SEARCH c USING")
	assert.NotContains(plan, "SCAN c")
}
