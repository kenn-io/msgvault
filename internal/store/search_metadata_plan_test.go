package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchMetadataSnippetQueryPlan(t *testing.T) {
	assert := assert.New(t)
	req := require.New(t)
	db := newFTSProbeDB(t, false)
	query := fmt.Sprintf(searchMetadataSnippetSQL, "?, ?, ?")
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, `"project"* "meeting"*`, 1, 3, 1, 2, 3)
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
