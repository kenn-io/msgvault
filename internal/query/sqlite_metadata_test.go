package query

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// Wrapping the dialect selects the historical scan path without changing its
// SQL semantics. EXPLAIN separately proves the other engine uses trigram MATCH.
type metadataScanDialect struct{ SQLiteQueryDialect }

func metadataSearchDB(tb testing.TB) *sql.DB {
	tb.Helper()
	st, err := store.OpenForTest(filepath.Join(tb.TempDir(), "metadata.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, st.Close()) })
	require.NoError(tb, st.InitSchema())
	_, err = st.DB().Exec(`INSERT INTO sources(id,source_type,identifier) VALUES(1,'gmail','archive@example.org');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,1,'synthetic-thread','email_thread');
 INSERT INTO participants(id,email_address,display_name,phone_number) VALUES(1,'contact@example.org','ÉCOLE Contact','+15550001111');
 INSERT INTO messages(id,conversation_id,source_id,message_type,source_message_id,subject,snippet,sent_at,sender_id)
 VALUES(1,1,1,'email','synthetic-1','prefix Needle suffix','preview','2024-01-01',NULL),
 (2,1,1,'email','synthetic-2','other','snippet needle','2024-01-02',NULL),
 (3,1,1,'email','synthetic-3','direct sender','other','2024-01-03',1),
 (4,1,1,'email','synthetic-4','recipient','other','2024-01-04',NULL);
 INSERT INTO message_recipients(id,message_id,participant_id,recipient_type,display_name) VALUES(1,4,1,'to','Occurrence Alias');`)
	require.NoError(tb, err)
	return st.DB()
}

func metadataPlan(t *testing.T, e *SQLiteEngine, q *search.Query) string {
	t.Helper()
	conditions, args, join := e.buildFilteredMetadataSearchQueryParts(t.Context(), q, MessageFilter{})
	rows, err := e.db.Query("EXPLAIN QUERY PLAN "+searchResultsSQL(join, strings.Join(conditions, " AND ")), append(args, 50, 0)...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(t, rows.Err())
	return plan.String()
}

func TestMetadataSearchIndexedPlan(t *testing.T) {
	assert := assert.New(t)
	e := NewSQLiteEngine(metadataSearchDB(t))
	for _, q := range []*search.Query{{TextTerms: []string{"eedle"}}, {TextTerms: []string{"ÉCOLE"}, AccountIDs: []int64{1}}} {
		plan := metadataPlan(t, e, q)
		assert.Contains(plan, "messages_metadata_fts VIRTUAL TABLE INDEX")
		assert.Contains(plan, "participants_metadata_fts VIRTUAL TABLE INDEX")
		assert.Contains(plan, "recipients_metadata_fts VIRTUAL TABLE INDEX")
		if len(q.AccountIDs) == 0 {
			assert.Contains(plan, "SEARCH m USING INTEGER PRIMARY KEY")
		} else {
			assert.Contains(plan, "source_id=? AND rowid=?")
		}
	}
	for _, q := range []*search.Query{search.Parse("after:2024-01-01 needle"), search.Parse("from:contact@example.org needle"), search.Parse("conversation_id:1 needle"), {TextTerms: []string{"ee"}}} {
		assert.NotContains(metadataPlan(t, e, q), "messages_metadata_fts VIRTUAL TABLE INDEX")
	}
}

func TestMetadataSearchLiteralFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	e := NewSQLiteEngine(metadataSearchDB(t))
	for _, tc := range []struct {
		term string
		want []int64
	}{
		{"eedle", []int64{2, 1}}, {"ÉCOLE", []int64{4, 3}}, {"00011", []int64{4, 3}}, {"rrence", []int64{4}},
	} {
		got, err := e.SearchFast(t.Context(), &search.Query{TextTerms: []string{tc.term}}, MessageFilter{}, 50, 0)
		require.NoError(err)
		var ids []int64
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		assert.Equal(tc.want, ids, tc.term)
		count, err := e.SearchFastCount(t.Context(), &search.Query{TextTerms: []string{tc.term}}, MessageFilter{})
		require.NoError(err)
		assert.Equal(int64(len(tc.want)), count)
	}
}

func TestMetadataSearchUnavailableIndex(t *testing.T) {
	for _, damage := range []string{`DROP TABLE recipients_metadata_fts`, `DELETE FROM archive_metadata WHERE key='metadata_fts_version'`, `UPDATE archive_metadata SET value='0' WHERE key='metadata_fts_version'`} {
		t.Run(damage, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			db := metadataSearchDB(t)
			_, err := db.Exec(damage)
			require.NoError(err)
			e := NewSQLiteEngine(db)
			assert.NotContains(metadataPlan(t, e, &search.Query{TextTerms: []string{"needle"}}), "messages_metadata_fts VIRTUAL TABLE INDEX")
			got, err := e.SearchFast(t.Context(), &search.Query{TextTerms: []string{"needle"}}, MessageFilter{}, 50, 0)
			require.NoError(err)
			assert.Len(got, 2)
		})
	}
	db := metadataSearchDB(t)
	e := NewSQLiteEngine(db)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.SearchFast(ctx, &search.Query{TextTerms: []string{"needle"}}, MessageFilter{}, 50, 0)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, metadataPlan(t, e, &search.Query{TextTerms: []string{"needle"}}), "messages_metadata_fts VIRTUAL TABLE INDEX", "cancelled probe must retry")
}

// The real SQLite authorizer lets cancellation land at a particular query
// phase without a throughput race or a fake database implementation.
func TestMetadataSearchCancellationDuringAggregates(t *testing.T) {
	for _, phase := range []string{"count", "sum"} {
		t.Run(phase, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			db := metadataSearchDB(t)
			db.SetMaxOpenConns(1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			conn, err := db.Conn(t.Context())
			require.NoError(err)
			fired := false
			require.NoError(conn.Raw(func(driverConn any) error {
				sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
				require.True(ok)
				sqliteConn.RegisterAuthorizer(func(op int, _, function, _ string) int {
					if op == sqlite3.SQLITE_FUNCTION && function == phase {
						fired = true
						cancel()
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			require.NoError(conn.Close())
			e := NewEngineWithDialect(db, metadataScanDialect{})
			got, err := e.SearchFastWithStats(ctx, &search.Query{TextTerms: []string{"needle"}}, "", MessageFilter{}, ViewSenders, 50, 0)
			assert.True(fired, "real count/stat statement must run")
			assert.Nil(got, "cancellation must not look like partial success")
			assert.ErrorIs(err, context.Canceled)
		})
	}
}

func TestMetadataSearchDeadlineError(t *testing.T) {
	e := NewSQLiteEngine(metadataSearchDB(t))
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	for _, call := range []func() error{
		func() error {
			_, err := e.SearchFast(ctx, &search.Query{TextTerms: []string{"ab"}}, MessageFilter{}, 50, 0)
			return err
		},
		func() error {
			_, err := e.SearchFastCount(ctx, &search.Query{TextTerms: []string{"ab"}}, MessageFilter{})
			return err
		},
		func() error {
			_, err := e.SearchFastWithStats(ctx, &search.Query{TextTerms: []string{"ab"}}, "", MessageFilter{}, ViewSenders, 50, 0)
			return err
		},
	} {
		err := call()
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorContains(t, err, "narrow")
	}
}

func TestMetadataSearchDeadlineBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		engine := NewSQLiteEngine(nil)
		start := time.Now()
		ctx, cancel := engine.metadataSearchContext(t.Context(), &search.Query{TextTerms: []string{"ab"}})
		defer cancel()
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		assert.Equal(start.Add(10*time.Second), deadline)
		parent, parentCancel := context.WithTimeout(t.Context(), time.Second)
		defer parentCancel()
		child, childCancel := engine.metadataSearchContext(parent, &search.Query{TextTerms: []string{"needle"}})
		defer childCancel()
		got, _ := child.Deadline()
		want, _ := parent.Deadline()
		assert.Equal(want, got)
		plain, plainCancel := engine.metadataSearchContext(t.Context(), &search.Query{})
		defer plainCancel()
		_, ok = plain.Deadline()
		assert.False(ok, "filter-only searches keep existing budgets")
		pg := NewEngineWithDialect(nil, PostgreSQLQueryDialect{})
		pgCtx, pgCancel := pg.metadataSearchContext(t.Context(), &search.Query{TextTerms: []string{"needle"}})
		defer pgCancel()
		_, ok = pgCtx.Deadline()
		assert.False(ok, "PostgreSQL keeps its own statement timeout")
	})
}
