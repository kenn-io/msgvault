package store

import (
	"database/sql"
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgreSQLDialect_Rebind(t *testing.T) {
	d := &PostgreSQLDialect{}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty query",
			in:   "",
			want: "",
		},
		{
			name: "no placeholders",
			in:   "SELECT 1",
			want: "SELECT 1",
		},
		{
			name: "single placeholder",
			in:   "SELECT * FROM t WHERE id = ?",
			want: "SELECT * FROM t WHERE id = $1",
		},
		{
			name: "multiple placeholders",
			in:   "INSERT INTO t (a, b, c) VALUES (?, ?, ?)",
			want: "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)",
		},
		{
			name: "placeholder inside quoted string is not converted",
			in:   "SELECT * FROM t WHERE name = 'what?' AND id = ?",
			want: "SELECT * FROM t WHERE name = 'what?' AND id = $1",
		},
		{
			name: "multiple quoted strings",
			in:   "SELECT * FROM t WHERE a = 'foo?' AND b = 'bar?' AND c = ?",
			want: "SELECT * FROM t WHERE a = 'foo?' AND b = 'bar?' AND c = $1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, d.Rebind(tc.in), "Rebind(%q)", tc.in)
		})
	}
}

func TestPostgreSQLDialect_InsertOrIgnore(t *testing.T) {
	d := &PostgreSQLDialect{}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "complete statement gets ON CONFLICT DO NOTHING",
			in:   "INSERT OR IGNORE INTO t (a) VALUES (?)",
			want: "INSERT INTO t (a) VALUES (?) ON CONFLICT DO NOTHING",
		},
		{
			name: "multi-value complete statement",
			in:   "INSERT OR IGNORE INTO t (a, b) VALUES (?, ?)",
			want: "INSERT INTO t (a, b) VALUES (?, ?) ON CONFLICT DO NOTHING",
		},
		{
			name: "prefix-only (ends with VALUES ) leaves suffix to caller",
			in:   "INSERT OR IGNORE INTO message_labels (message_id, label_id) VALUES ",
			want: "INSERT INTO message_labels (message_id, label_id) VALUES ",
		},
		{
			name: "INSERT ... SELECT gets ON CONFLICT DO NOTHING",
			in:   "INSERT OR IGNORE INTO collection_sources (collection_id, source_id) SELECT ?, id FROM sources",
			want: "INSERT INTO collection_sources (collection_id, source_id) SELECT ?, id FROM sources ON CONFLICT DO NOTHING",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, d.InsertOrIgnore(tc.in), "InsertOrIgnore(%q)", tc.in)
		})
	}
}

type recordingFTSQuerier struct {
	query string
	args  []any
	all   []string
}

func TestPostgreSQLDialect_FTSSearchClause(t *testing.T) {
	assert := assert.New(t)
	d := &PostgreSQLDialect{}
	join, where, orderBy, orderArgCount := d.FTSSearchClause(false, []string{"foo-bar"})
	assert.Empty(join)
	assert.Contains(where, "m.search_fts @@ to_tsquery('simple', ?)")
	assert.Contains(where, "foo:A <-> bar:*A")
	assert.Equal("-ts_rank(ARRAY[0.1, 0.1, 0.4, 1.0]::real[], m.search_fts, to_tsquery('simple', ?))", orderBy)
	assert.Equal(1, orderArgCount)
}

func TestPostgreSQLDialect_BuildFTSArg(t *testing.T) {
	d := &PostgreSQLDialect{}
	for _, tc := range []struct {
		terms []string
		want  string
	}{
		{[]string{"invoice"}, "invoice:*"},
		{[]string{"invoice", "review"}, "invoice:* & review:*"},
		{[]string{"---"}, ""},
		{[]string{"foo-bar"}, "foo & bar:*"},
		{[]string{"user@example.com"}, "user & example & com:*"},
		{[]string{"a.b.c"}, "a & b & c:*"},
		{[]string{"invoice", "foo-bar"}, "invoice:* & foo & bar:*"},
		{[]string{"monthly", "foo-bar", "or"}, "monthly:* & foo & bar:* & or:*"},
	} {
		assert.Equal(t, tc.want, d.BuildFTSArg(tc.terms))
	}
}

func (q *recordingFTSQuerier) Exec(query string, args ...any) (sql.Result, error) {
	q.query = query
	q.args = args
	q.all = append(q.all, query)
	return driver.RowsAffected(1), nil
}

func (*recordingFTSQuerier) QueryRow(string, ...any) *sql.Row {
	panic("QueryRow is not used by FTSUpsert")
}

func TestPostgreSQLDialect_FTSLayoutVersioned(t *testing.T) {
	assert := assert.New(t)
	d := &PostgreSQLDialect{}
	q := &recordingFTSQuerier{}
	require.NoError(t, d.FTSUpsert(q, FTSDoc{
		MessageID: 1,
		Subject:   "subject",
		Body:      "body",
		FromAddr:  "from@example.com",
		ToAddrs:   "to@example.com",
		CcAddrs:   "cc@example.com",
	}), "FTSUpsert")

	assert.Contains(q.query, "setweight(to_tsvector('simple', LEFT(COALESCE($2, '')", "subject vector")
	assert.Contains(q.query, "), 'A')", "subject weight")
	assert.Contains(q.query, "setweight(to_tsvector('simple', LEFT(COALESCE($4, '')", "sender vector")
	assert.Contains(q.query, "), 'B')", "sender weight")
	assert.Contains(q.query, "setweight(to_tsvector('simple', LEFT(COALESCE($5, '')", "to vector")
	assert.Contains(q.query, "setweight(to_tsvector('simple', LEFT(COALESCE($6, '')", "cc vector")
	assert.Contains(q.query, "), 'C')", "recipient weight")
	assert.Contains(q.query, "setweight(to_tsvector('simple', LEFT(COALESCE($3, '')", "body vector")
	assert.Contains(q.query, "), 'D')", "body weight")
	assert.Contains(q.query, "indexing_version = 3", "layout version stamp")
}

func TestPostgreSQLDialect_FTSNeedsBackfillSQLUsesLiteralVersion(t *testing.T) {
	sql := postgresFTSNeedsBackfillSQL()
	assert.Contains(t, sql, "indexing_version IS DISTINCT FROM 3")
	assert.NotContains(t, sql, "$1", "partial-index predicate must remain plan-time provable")
}

func TestPostgreSQLDialect_EnsureFTSIndexUsesVersionedStalePredicate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := &PostgreSQLDialect{}
	q := &recordingFTSQuerier{}
	require.NoError(d.EnsureFTSIndex(q), "EnsureFTSIndex")
	require.Len(q.all, 2, "GIN and stale-row indexes")
	assert.Contains(q.all[1], "idx_messages_search_fts_stale_v3")
	assert.Contains(q.all[1], "search_fts IS NULL OR indexing_version IS DISTINCT FROM 3")
}

func TestPostgreSQLDialect_InsertOrIgnorePrefix(t *testing.T) {
	d := &PostgreSQLDialect{}
	in := "INSERT OR IGNORE INTO message_labels (message_id, label_id) VALUES "
	want := "INSERT INTO message_labels (message_id, label_id) VALUES "
	assert.Equal(t, want, d.InsertOrIgnorePrefix(in), "InsertOrIgnorePrefix(%q)", in)
}
