package store_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOperationInvocationSchemasExposeOnlyAllowlistedColumns(t *testing.T) {
	st := testutil.NewTestStore(t)
	common := []string{"id", "invocation_key", "trigger", "state", "started_at", "finished_at", "error_code", "attempted", "succeeded", "failed"}
	want := map[string][]string{
		"message_embedding_runs":   append(slices.Clone(common), "truncated"),
		"person_embedding_runs":    append(slices.Clone(common), "truncated"),
		"document_extraction_runs": slices.Clone(common),
		"document_embedding_runs":  slices.Clone(common),
		"visual_embedding_runs":    append(slices.Clone(common), "skipped"),
		"operation_token_keys":     {"key_id", "key_bytes", "state", "created_at", "retired_at"},
	}
	for table, expected := range want {
		t.Run(table, func(t *testing.T) {
			assert.Equal(t, expected, operationTableColumns(t, st, table))
		})
	}
}

func operationTableColumns(t *testing.T, st interface {
	IsPostgreSQL() bool
	DB() *sql.DB
	Rebind(query string) string
}, table string) []string {
	t.Helper()
	query := `SELECT name FROM pragma_table_info(?) ORDER BY cid`
	if st.IsPostgreSQL() {
		query = `SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? ORDER BY ordinal_position`
	}
	rows, err := st.DB().Query(st.Rebind(query), table)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())
	return columns
}

func TestOperationRunOrderIndexes(t *testing.T) {
	st := testutil.NewTestStore(t)

	type expectedIndex struct {
		table   string
		columns []string
	}
	want := map[string]expectedIndex{
		"idx_sync_runs_operations_order":                {table: "sync_runs", columns: []string{"started_at", "id"}},
		"idx_person_sweep_runs_operations_order":        {table: "person_sweep_runs", columns: []string{"started_at", "id"}},
		"idx_carddav_sync_runs_operations_order":        {table: "carddav_sync_runs", columns: []string{"started_at", "id"}},
		"idx_message_embedding_runs_operations_order":   {table: "message_embedding_runs", columns: []string{"started_at", "id"}},
		"idx_person_embedding_runs_operations_order":    {table: "person_embedding_runs", columns: []string{"started_at", "id"}},
		"idx_document_extraction_runs_operations_order": {table: "document_extraction_runs", columns: []string{"started_at", "id"}},
		"idx_document_embedding_runs_operations_order":  {table: "document_embedding_runs", columns: []string{"started_at", "id"}},
		"idx_visual_embedding_runs_operations_order":    {table: "visual_embedding_runs", columns: []string{"started_at", "id"}},
	}
	for indexName, expected := range want {
		t.Run(indexName, func(t *testing.T) {
			if st.IsPostgreSQL() {
				assertPostgresDescendingIndex(t, st.DB(), expected.table, indexName, expected.columns)
				return
			}
			assertSQLiteDescendingIndex(t, st.DB(), expected.table, indexName, expected.columns)
		})
	}
}

func TestOperationLaneStatusIndexesFilterOnStatus(t *testing.T) {
	st := testutil.NewTestStore(t)
	tests := []struct {
		index    string
		sqlite   string
		postgres string
	}{
		{
			index:    "idx_sync_runs_operations_running",
			sqlite:   "WHERE status = 'running'",
			postgres: "WHERE (status = 'running'::text)",
		},
		{
			index:    "idx_sync_runs_operations_succeeded",
			sqlite:   "WHERE status = 'completed' AND errors_count = 0",
			postgres: "WHERE ((status = 'completed'::text) AND (errors_count = 0))",
		},
		{
			index:    "idx_person_sweep_runs_operations_running",
			sqlite:   "WHERE status = 'running'",
			postgres: "WHERE (status = 'running'::text)",
		},
		{
			index:    "idx_person_sweep_runs_operations_succeeded",
			sqlite:   "WHERE status = 'succeeded'",
			postgres: "WHERE (status = 'succeeded'::text)",
		},
	}
	for _, test := range tests {
		t.Run(test.index, func(t *testing.T) {
			if st.IsPostgreSQL() {
				assertPostgresIndexDefinitionContains(t, st.DB(), test.index, test.postgres)
				return
			}
			assertSQLiteIndexDefinitionContains(t, st.DB(), test.index, test.sqlite)
		})
	}
}

func TestPersonSweepOperationIndexesOwnBytewiseOrdering(t *testing.T) {
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		assertPostgresIndexDefinitionContains(t, st.DB(),
			"idx_person_sweep_runs_operations_bytewise_order", `id COLLATE "C" DESC`)
		assertPostgresIndexDefinitionContains(t, st.DB(),
			"idx_person_sweep_attempts_operations_failure", `COALESCE(completed_at, started_at)`)
		assertPostgresIndexDefinitionContains(t, st.DB(),
			"idx_person_sweep_attempts_operations_failure", `id COLLATE "C" DESC`)
		return
	}
	assertSQLiteIndexDefinitionContains(t, st.DB(),
		"idx_person_sweep_runs_operations_bytewise_order", "id COLLATE BINARY DESC")
	assertSQLiteIndexDefinitionContains(t, st.DB(),
		"idx_person_sweep_attempts_operations_failure", "COALESCE(completed_at, started_at) DESC")
	assertSQLiteIndexDefinitionContains(t, st.DB(),
		"idx_person_sweep_attempts_operations_failure", "id COLLATE BINARY DESC")
}

func assertPostgresIndexDefinitionContains(
	t *testing.T, db queryRower, indexName, fragment string,
) {
	t.Helper()
	var definition string
	err := db.QueryRow(`SELECT pg_get_indexdef(indexname::regclass)
		FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1`, indexName).Scan(&definition)
	require.NoError(t, err)
	assert.Contains(t, definition, fragment)
}

func assertSQLiteIndexDefinitionContains(
	t *testing.T, db *sql.DB, indexName, fragment string,
) {
	t.Helper()
	var definition string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, indexName).Scan(&definition)
	require.NoError(t, err)
	assert.Contains(t, definition, fragment)
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func assertPostgresDescendingIndex(t *testing.T, db queryRower, tableName, indexName string, columns []string) {
	t.Helper()
	var actualTable, definition string
	err := db.QueryRow(`SELECT tablename, pg_get_indexdef(indexname::regclass)
		FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = $1`, indexName).Scan(&actualTable, &definition)
	require.NoError(t, err)
	assert.Equal(t, tableName, actualTable)
	assert.Contains(t, definition, fmt.Sprintf("(%s DESC)", strings.Join(columns, " DESC, ")))
}

func assertSQLiteDescendingIndex(t *testing.T, db *sql.DB, tableName, indexName string, columns []string) {
	t.Helper()
	indexRows, err := db.Query("PRAGMA index_list(" + tableName + ")")
	require.NoError(t, err)
	defer func() { require.NoError(t, indexRows.Close()) }()
	owned := false
	for indexRows.Next() {
		var sequence, unique, partial int
		var name, origin string
		require.NoError(t, indexRows.Scan(&sequence, &name, &unique, &origin, &partial))
		owned = owned || name == indexName
	}
	require.NoError(t, indexRows.Err())
	require.True(t, owned, "%s must own %s", tableName, indexName)

	type indexColumn struct {
		name string
		desc int
	}
	rows, err := db.Query("PRAGMA index_xinfo(" + indexName + ")")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	got := make([]indexColumn, 0, len(columns))
	for rows.Next() {
		var seqno, cid, descending, key int
		var name, collation any
		require.NoError(t, rows.Scan(&seqno, &cid, &name, &descending, &collation, &key))
		if key == 0 {
			continue
		}
		columnName, ok := name.(string)
		require.True(t, ok)
		got = append(got, indexColumn{name: columnName, desc: descending})
	}
	require.NoError(t, rows.Err())

	want := make([]indexColumn, 0, len(columns))
	for _, column := range columns {
		want = append(want, indexColumn{name: column, desc: 1})
	}
	assert.Equal(t, want, got)
}
