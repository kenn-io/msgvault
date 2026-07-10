package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentPackLivenessQueriesUseExpressionIndexes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	st, err := OpenForTest(filepath.Join(dir, "liveness.db"))
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	require.NoError(st.InitSchema())

	hash := strings.Repeat("a", 64)
	tests := map[string]struct {
		plan        string
		indexPrefix string
	}{
		"resolve": {
			plan:        explainPlan(t, st, resolveAttachmentBlobSQL, hash, hash, hash),
			indexPrefix: "SEARCH a USING COVERING INDEX ",
		},
		"prune": {
			plan:        explainPlan(t, st, pruneUnreferencedPackIndexSQL),
			indexPrefix: "SCAN attachments USING INDEX ",
		},
		"usage": {
			plan:        explainPlan(t, st, listPackUsageSQL),
			indexPrefix: "SCAN attachments USING INDEX ",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Logf("query plan:\n%s", tc.plan)
			assert.Contains(tc.plan, tc.indexPrefix+"idx_attachments_content_hash_lower",
				"content liveness must use its expression index:\n%s", tc.plan)
			assert.Contains(tc.plan, tc.indexPrefix+"idx_attachments_thumbnail_hash_lower",
				"thumbnail liveness must use its expression index:\n%s", tc.plan)
			assert.NotContains(tc.plan, "CORRELATED", "liveness must not rescan attachments per mapping:\n%s", tc.plan)
		})
	}
}
