package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSearchMetadataSnippetQueryPlan(t *testing.T) {
	dbtest.SkipIfPostgres(t, "match excerpts use SQLite FTS5")
	assert := assert.New(t)
	req := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "snippet-plan.db"))
	req.NoError(err)
	t.Cleanup(func() { req.NoError(st.Close()) })
	req.NoError(st.InitSchema())
	query := fmt.Sprintf(searchMetadataSnippetSQL, "?, ?, ?")
	rows, err := st.DB().Query("EXPLAIN QUERY PLAN "+query, `"project"* "meeting"*`, 1, 3, 1, 2, 3)
	req.NoError(err)
	defer func() { req.NoError(rows.Close()) }()
	var ftsScans int
	for rows.Next() {
		var id, parent, unused int
		var detail string
		req.NoError(rows.Scan(&id, &parent, &unused, &detail))
		if strings.Contains(detail, "SCAN messages_fts VIRTUAL TABLE INDEX") {
			ftsScans++
			_, constraints, ok := strings.Cut(detail, ":")
			req.True(ok, "FTS scan must report its constraints: %s", detail)
			assert.Contains(constraints, "M", "FTS scan must use MATCH: %s", detail)
			assert.Contains(constraints, ">", "FTS scan must use the lower rowid bound: %s", detail)
			assert.Contains(constraints, "<", "FTS scan must use the upper rowid bound: %s", detail)
			assert.NotContains(constraints, "=", "FTS scan must avoid one MATCH per ID: %s", detail)
		}
	}
	req.NoError(rows.Err())
	assert.Positive(ftsScans, "query must scan the full-text index")
}
