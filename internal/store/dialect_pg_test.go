package store

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
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
			assertpkg.Equal(t, tc.want, d.Rebind(tc.in), "Rebind(%q)", tc.in)
		})
	}
}

func TestPostgreSQLDialect_Now(t *testing.T) {
	d := &PostgreSQLDialect{}
	assertpkg.Equal(t, "NOW()", d.Now())
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
			assertpkg.Equal(t, tc.want, d.InsertOrIgnore(tc.in), "InsertOrIgnore(%q)", tc.in)
		})
	}
}

func TestPostgreSQLDialect_InsertOrIgnoreSuffix(t *testing.T) {
	d := &PostgreSQLDialect{}
	assertpkg.Equal(t, " ON CONFLICT DO NOTHING", d.InsertOrIgnoreSuffix())
}

func TestPostgreSQLDialect_FTSSearchClause(t *testing.T) {
	assert := assertpkg.New(t)
	d := &PostgreSQLDialect{}
	join, where, orderBy, orderArgCount := d.FTSSearchClause()
	assert.Empty(join, "join (PostgreSQL needs no JOIN)")
	assert.Equal("m.search_fts @@ to_tsquery('simple', ?)", where)
	assert.Equal("ts_rank(m.search_fts, to_tsquery('simple', ?)) DESC", orderBy)
	assert.Equal(1, orderArgCount, "orderArgCount (ts_rank needs query a second time)")
}

// TestPostgreSQLDialect_BuildFTSArg covers R3: the tsquery argument
// builder must split user terms on punctuation so inputs like `---`,
// `foo-bar`, `user@example.com`, and `a.b.c` produce only safe
// letter/digit lexemes rather than something to_tsquery would reject.
// The complementary integration test that actually feeds these
// strings into PG lives in pg_compat_test.go as
// TestSearchMessages_R3PunctuationTerms.
func TestPostgreSQLDialect_BuildFTSArg(t *testing.T) {
	d := &PostgreSQLDialect{}
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"plain", []string{"invoice"}, "invoice:*"},
		{"two_plain", []string{"invoice", "review"}, "invoice:* & review:*"},
		{"dashes_only_drops", []string{"---"}, ""},
		{"hyphenated_splits", []string{"foo-bar"}, "foo:* & bar:*"},
		{"email_splits",
			[]string{"user@example.com"},
			"user:* & example:* & com:*"},
		{"dotted_acronym_splits",
			[]string{"a.b.c"}, "a:* & b:* & c:*"},
		{"mix_of_clean_and_punct",
			[]string{"invoice", "foo-bar"},
			"invoice:* & foo:* & bar:*"},
		{"only_punct_collapses_to_empty",
			[]string{"---", "..."}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertpkg.Equal(t, tc.want, d.BuildFTSArg(tc.in), "BuildFTSArg(%q)", tc.in)
		})
	}
}

func TestPostgreSQLDialect_InsertOrIgnorePrefix(t *testing.T) {
	d := &PostgreSQLDialect{}
	in := "INSERT OR IGNORE INTO message_labels (message_id, label_id) VALUES "
	want := "INSERT INTO message_labels (message_id, label_id) VALUES "
	assertpkg.Equal(t, want, d.InsertOrIgnorePrefix(in), "InsertOrIgnorePrefix(%q)", in)
}
