//go:build cgo

package query_test

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSQLiteListThreadUsesExpressionIndex(t *testing.T) {
	t.Parallel()
	must := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite query-plan regression")
	}
	f.CreateMessage("indexed-thread-page")
	// Most messages belong to another thread, so the conversation filter
	// selects fewer rows than the source filter after ANALYZE.
	otherConv, err := f.Store.EnsureConversation(f.Source.ID, "other-thread", "Other thread")
	must.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), `
		WITH RECURSIVE n(id) AS (SELECT 1 UNION ALL SELECT id + 1 FROM n WHERE id < 1000)
		INSERT INTO messages (source_id, conversation_id, source_message_id, message_type, sent_at)
		SELECT ?, CASE WHEN id <= 100 THEN ? ELSE ? END, 'thread-plan-' || id, 'email', datetime('2026-01-15', '+' || id || ' seconds') FROM n
	`, f.Source.ID, f.ConvID, otherConv)
	must.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), "ANALYZE")
	must.NoError(err)
	// Observe queries against the same temporary archive, including its indexes
	// and planner statistics. Unqualified table names resolve to the attachment.
	var sequence int
	var name, path string
	must.NoError(f.Store.DB().QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&sequence, &name, &path))
	db, observer := sqlobserve.Open(t, func(conn *sqlite3.SQLiteConn) error {
		if _, err := conn.Exec("ATTACH DATABASE ? AS archive", []driver.Value{path}); err != nil {
			return fmt.Errorf("attach test archive: %w", err)
		}
		return nil
	})
	engine := query.NewSQLiteEngine(db)

	for _, scoped := range []bool{false, true} {
		q := query.ThreadQuery{ThreadID: "default-thread", Limit: 10, Offset: 90}
		args := []any{f.ConvID}
		if scoped {
			q.SourceIDs = []int64{f.Source.ID}
			args = append(args, f.Source.ID)
		}
		observer.Reset()
		_, err := engine.ListThread(t.Context(), q)
		must.NoError(err)
		var checked int
		for _, statement := range observer.Statements() {
			sql := strings.Join(strings.Fields(statement), " ")
			if !strings.Contains(sql, "FROM messages m WHERE m.conversation_id = ?") || !strings.Contains(sql, "ORDER BY") {
				continue
			}
			checked++
			t.Run(fmt.Sprintf("page/scoped=%t", scoped), func(t *testing.T) {
				assert := assert.New(t)
				queryArgs := append([]any{}, args...)
				queryArgs = append(queryArgs, q.Limit, q.Offset)
				plan := dbtest.ExplainQueryPlan(t, db, statement, queryArgs...)
				assert.Contains(plan, "idx_messages_conversation_sent_at_julianday", plan)
				assert.NotContains(plan, "TEMP B-TREE", plan)
			})
		}
		checks.Equal(1, checked, "observe ListThread's page statement")
	}
}
