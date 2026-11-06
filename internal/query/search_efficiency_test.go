//go:build cgo

package query

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

func observedSearchEngine(t *testing.T) (*SQLiteEngine, *sqlobserve.Observer) {
	t.Helper()
	db, observer := sqlobserve.Open(t, nil)
	schema, err := os.ReadFile("../store/schema.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(schema))
	require.NoError(t, err)
	fixture := &dbtest.TestDB{DB: db, T: t}
	fixture.SeedStandardDataSet()
	observer.Reset()
	return NewSQLiteEngine(db), observer
}

func TestSearchCommonWordUsesDateWindow(t *testing.T) {
	t.Parallel()
	engine, observer := observedSearchEngine(t)
	_, err := engine.db.Exec(`WITH RECURSIVE seq(x) AS (
		VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x < 3000
	) INSERT INTO messages (id, source_message_id, source_id, conversation_id, message_type, sent_at, subject)
	SELECT 100+x, 'window-' || x, 1, 1, 'email', datetime('2025-01-01', '+' || x || ' minutes'),
		CASE WHEN x % 4 != 0 THEN 'invoice' ELSE 'budget' END ||
		CASE WHEN x <= 1500 THEN ' oldest' ELSE '' END ||
		CASE WHEN x <= 1279 THEN ' belowcap' ELSE '' END ||
		CASE WHEN x <= 1280 THEN ' atcap' ELSE '' END FROM seq`)
	require.NoError(t, err)
	(&dbtest.TestDB{DB: engine.db, T: t}).EnableFTS()
	for _, tc := range []struct {
		name, text                string
		accounts                  []int64
		scope                     search.DeletionScope
		limit, offset, statements int
		walk                      bool
	}{
		{name: "common", text: "invoice", limit: 7, statements: 1, walk: true},
		{name: "account", text: "invoice", accounts: []int64{1}, limit: 100, statements: 1},
		{name: "OR", text: "invoice | budget", limit: 100, statements: 1},
		{name: "deleted", text: "invoice", scope: search.DeletionScopeDeleted, limit: 100, statements: 1},
		{name: "deep page", text: "invoice", limit: 100, offset: 5000, statements: 1},
		{name: "old only", text: "oldest", limit: 7, statements: 2},
		{name: "below window cap", text: "belowcap", limit: 10, statements: 1},
		{name: "at window cap", text: "atcap", limit: 10, statements: 2},
		{name: "no matches", text: "zzqx", limit: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			q := search.ParseOR(tc.text)
			q.AccountIDs, q.DeletionScope = tc.accounts, tc.scope
			observer.Reset()
			_, err := engine.Search(t.Context(), q, tc.limit, tc.offset)
			require.NoError(err)
			var pages []string
			matches := 0
			for _, statement := range observer.Statements() {
				matches += strings.Count(statement, "messages_fts MATCH")
				if strings.Contains(statement, "LIMIT ? OFFSET ?") {
					pages = append(pages, statement)
				}
			}
			require.Len(pages, tc.statements)
			if tc.statements == 0 {
				assert.Equal(1, matches)
				return
			}
			if tc.statements == 2 {
				assert.Contains(pages[0], "INDEXED BY idx_messages_sent_at")
			}
			statement := pages[len(pages)-1]
			plan := dbtest.ExplainQueryPlan(t, engine.db, statement, make([]any, strings.Count(statement, "?"))...)
			if tc.walk {
				index := strings.Index(plan, "SEARCH m USING INDEX idx_messages_sent_at")
				require.GreaterOrEqual(index, 0, plan)
				assert.Less(index, strings.Index(plan, "SCAN page"), plan)
			} else {
				assert.NotContains(plan, "SEARCH m USING INDEX idx_messages_sent_at")
			}
		})
	}
}

func TestMetadataSearchSQLiteOptionalStats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, query                     string
		limit, offset, rows, statements int
		total                           int64
	}{
		{"page", "after:2024-01-01", 2, 0, 2, 3, 5},
		{"end", "after:2024-01-01", 2, 9, 0, 2, 5},
		{"empty", "after:2099-01-01", 2, 0, 0, 2, 0},
		{"count only", "after:2024-01-01", 0, 0, 0, 1, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			engine, observer := observedSearchEngine(t)
			result, err := engine.SearchFastWithStats(t.Context(), search.Parse(tc.query), tc.query, MessageFilter{}, ViewNoStats, tc.limit, tc.offset)
			require.NoError(err)
			assert.Equal(tc.total, result.TotalCount)
			assert.Len(result.Messages, tc.rows)
			assert.Nil(result.Stats)
			assert.Len(observer.Statements(), tc.statements)
			for _, statement := range observer.Statements() {
				assert.NotContains(statement, "message_bodies")
			}
		})
	}
}
