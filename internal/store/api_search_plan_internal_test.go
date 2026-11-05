//go:build cgo

package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSearchFTSSelectsPageBeforeDetails(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite query plan")
	st, err := Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())
	plan := st.buildMessageSearchSQL(t.Context(), search.Parse("project"), true)
	statement, args := messageSearchPageSQL(plan, 0, 50)
	explain := dbtest.ExplainQueryPlan(t, st.db.DB, statement, args...)
	sort := strings.Index(explain, "USE TEMP B-TREE FOR ORDER BY")
	require.GreaterOrEqual(t, sort, 0, explain)
	for _, lookup := range []string{"SEARCH p USING INTEGER PRIMARY KEY", "SEARCH c USING INTEGER PRIMARY KEY"} {
		position := strings.Index(explain, lookup)
		require.GreaterOrEqual(t, position, 0, explain)
		assert.Less(t, sort, position, explain)
	}
}
