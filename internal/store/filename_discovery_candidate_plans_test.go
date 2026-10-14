package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/filenamediscovery"
)

type filenameObservedQuery struct {
	sql  string
	args []any
	rows int
}
type filenamePostingObserver struct {
	all     bool
	rows    int
	queries []filenameObservedQuery
}
type filenamePostingConnector struct {
	driver   driver.Driver
	dsn      string
	observer *filenamePostingObserver
}

func (c *filenamePostingConnector) Driver() driver.Driver { return c.driver }
func (c *filenamePostingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &filenamePostingConn{Conn: conn, observer: c.observer}, nil
}

// Forward all native connection contracts through the existing test wrapper.
// Only real emitted posting rows are observed: no canned SQL or fake data.
type filenamePostingConn struct {
	directorySnapshotConn

	observer *filenamePostingObserver
}

func (c *filenamePostingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := q.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	// Posting seeks are the only filename queries that read p.attachment_id.
	if c.observer != nil && (c.observer.all || strings.Contains(query, "p.attachment_id")) {
		values := make([]any, len(args))
		for i, arg := range args {
			values[i] = arg.Value
		}
		c.observer.queries = append(c.observer.queries, filenameObservedQuery{sql: query, args: values})
		return &filenamePostingRows{Rows: rows, observer: c.observer, queryIndex: len(c.observer.queries) - 1}, nil
	}
	return rows, nil
}
func (c *filenamePostingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if exec, ok := c.Conn.(driver.ExecerContext); ok {
		return exec.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

type filenamePostingRows struct {
	driver.Rows

	observer   *filenamePostingObserver
	queryIndex int
}

func (r *filenamePostingRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.observer.rows++
		r.observer.queries[r.queryIndex].rows++
	}
	return err
}

func filenameObservedStore(t *testing.T) (*Store, *filenamePostingObserver) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "posting.db")
	pg := IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB"))
	if pg {
		base, err := OpenForTest(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		schema := "filename_postings_" + strings.ToLower(rand.Text())
		_, err = base.DB().ExecContext(t.Context(), `CREATE SCHEMA `+schema)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := base.DB().Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
			assert.NoError(t, err)
			assert.NoError(t, base.Close())
		})
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		values := u.Query()
		values.Set("search_path", schema+",public")
		u.RawQuery = values.Encode()
		path = u.String()
	}
	st, err := OpenForTest(path)
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	observer := &filenamePostingObserver{}
	dsn := path
	if !pg {
		dsn += testSQLiteParams
	}
	db := sql.OpenDB(&filenamePostingConnector{driver: st.db.Driver(), dsn: dsn, observer: observer})
	db.SetMaxOpenConns(1)
	old := st.db.DB
	st.db = newLoggedDB(db, st.dialect.Rebind)
	require.NoError(t, old.Close())
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	return st, observer
}

// A counter wrapped around native Rows.Next catches free duplicates, page
// read-ahead, or an off-budget LIMIT1 even when final IDs look bounded.
func filenamePostingPlanFixture(t *testing.T) (*Store, *filenamePostingObserver) {
	t.Helper()
	require := require.New(t)

	st, observer := filenameObservedStore(t)
	source, err := st.GetOrCreateSource("imap", "postings@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "posting-plan", "Posting Example")
	require.NoError(err)
	tx, err := st.db.BeginTx(t.Context(), nil)
	require.NoError(err)
	var message int64
	require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) VALUES(?,?,'posting-plan','email') RETURNING id`, source.ID, conv).Scan(&message))
	state := filenameDiscoveryState{Generation: 1, Revision: 1, Phase: "ready"}
	updates := make([]filenameProjectionUpdate, 0, 3000)
	for i := 1; i <= 3000; i++ {
		name := "report.pdf"
		if i > 600 {
			name = fmt.Sprintf("synthetic%05d unrelated.pdf", i)
		}
		var id int64
		require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,?,'') RETURNING id`, message, name).Scan(&id))
		require.Equal(int64(i), id, "fresh fixture IDs keep the literal ranges below meaningful")
		updates = append(updates, filenameProjectionUpdate{id: id, currentName: name, currentFound: true})
	}
	require.NoError(st.filenameApplyBatchTx(t.Context(), tx, &state, updates))
	// A real unrelated population makes analyzed plans reflect point probes
	// rather than a cheap scan of one-row message and conversation tables.
	for i := range 2000 {
		var c int64
		require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,?,'email_thread') RETURNING id`, source.ID, fmt.Sprintf("unrelated-%d", i)).Scan(&c))
		_, err = tx.ExecContext(t.Context(), `INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) VALUES(?,?,?,'email')`, source.ID, c, fmt.Sprintf("unrelated-%d", i))
		require.NoError(err)
	}
	require.NoError(saveFilenameDiscoveryState(t.Context(), tx, state))
	_, err = tx.ExecContext(t.Context(), `INSERT INTO attachment_change_consumers(consumer_key,baseline_sequence,last_sequence,reconciliation_complete) VALUES(?,0,0,TRUE)`, filenameDiscoveryConsumer)
	require.NoError(err)
	require.NoError(tx.Commit())
	for _, table := range []string{"filename_discovery_entries", "filename_discovery_tokens", "filename_discovery_deletes", "attachments", "messages", "conversations"} {
		_, err = st.db.ExecContext(t.Context(), `ANALYZE `+table)
		require.NoError(err)
	}
	return st, observer
}

func TestFilenameCandidatesQueryPlans(t *testing.T) {
	st, observer := filenamePostingPlanFixture(t)
	for _, raw := range []string{"report.pdf", "repor.pdf", "reprot.pdf", "synthetic01234.pdf", "absent.pdf"} {
		t.Run(raw, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			observer.rows = 0
			observer.queries = nil
			q, err := filenamediscovery.ParseQuery(raw)
			require.NoError(err)
			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			require.NoError(err)
			set, err := st.FilenameCandidatesContext(ctx, q, FilenameSuggestionScope{})
			release()
			require.NoError(err)
			assert.Equal(observer.rows, set.PostingRows, "physical native rows, including duplicate/read-ahead pages")
			assert.LessOrEqual(observer.rows, 2048)
			assert.LessOrEqual(len(set.IDs), 500)
			queries := observer.queries
			observer.queries = nil
			seen := map[string]bool{}
			for _, query := range queries {
				densityKey := fmt.Sprintf("%s:dense=%t", query.sql, query.rows >= 32)
				if seen[densityKey] {
					continue
				}
				seen[densityKey] = true
				// Explain exactly the executed statement and bindings; no duplicate query builder.
				if st.IsPostgreSQL() {
					var rawPlan string
					require.NoError(st.DB().QueryRowContext(t.Context(), `EXPLAIN (FORMAT JSON) `+query.sql, query.args...).Scan(&rawPlan))
					var plan []map[string]any
					require.NoError(json.Unmarshal([]byte(rawPlan), &plan))
					var inspect func(map[string]any)
					indexed := false
					inspect = func(node map[string]any) {
						kind, _ := node["Node Type"].(string)
						assert.NotEqual("Sort", kind)
						assert.NotEqual("Seq Scan", kind)
						if strings.Contains(kind, "Index") {
							indexed = true
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								childPlan, ok := child.(map[string]any)
								require.True(ok)
								inspect(childPlan)
							}
						}
					}
					require.NotEmpty(plan)
					root, ok := plan[0]["Plan"].(map[string]any)
					require.True(ok)
					inspect(root)
					assert.True(indexed, rawPlan)
					t.Logf("native PG returned=%d query=%s plan=%s", query.rows, query.sql, rawPlan)
				} else {
					rows, err := st.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query.sql, query.args...)
					require.NoError(err)
					defer func() { require.NoError(rows.Close()) }()
					indexed := false
					for rows.Next() {
						var id, parent, unused int
						var detail string
						require.NoError(rows.Scan(&id, &parent, &unused, &detail))
						assert.NotContains(detail, "TEMP B-TREE")
						assert.NotContains(detail, "SCAN ")
						indexed = indexed || strings.Contains(detail, "SEARCH ")
						t.Logf("native SQLite returned=%d query=%s plan=%s", query.rows, query.sql, detail)
					}
					require.NoError(rows.Err())
					require.NoError(rows.Close())
					assert.True(indexed)
				}
			}
		})
	}
	t.Run("physical-duplicate-budget", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		// Keep 499 matching IDs so incompleteness can only come from the
		// physical posting budget, not the candidate count.
		_, err := st.db.ExecContext(t.Context(), `DELETE FROM filename_discovery_entries WHERE generation=1 AND attachment_id BETWEEN 500 AND 600`)
		require.NoError(err)
		observer.rows = 0
		observer.queries = nil
		q, err := filenamediscovery.ParseQuery("report.pdf")
		require.NoError(err)
		ctx, release, err := st.BeginReadSnapshotContext(t.Context())
		require.NoError(err)
		set, err := st.FilenameCandidatesContext(ctx, q, FilenameSuggestionScope{})
		release()
		require.NoError(err)
		assert.Equal(2048, observer.rows)
		assert.Equal(observer.rows, set.PostingRows)
		assert.Len(set.IDs, 499)
		assert.True(set.CandidateLimitReached)
	})
}
