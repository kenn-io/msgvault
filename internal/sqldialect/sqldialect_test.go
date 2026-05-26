package sqldialect

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRebindPostgreSQL(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"none", "SELECT 1", "SELECT 1"},
		{"one", "SELECT * FROM t WHERE a = ?", "SELECT * FROM t WHERE a = $1"},
		{"two", "INSERT INTO t (a, b) VALUES (?, ?)", "INSERT INTO t (a, b) VALUES ($1, $2)"},
		{"quoted_question_mark", "SELECT '? literal' FROM t WHERE x = ?",
			"SELECT '? literal' FROM t WHERE x = $1"},
		{"three_alternating",
			"SELECT * FROM t WHERE a = ? AND b = '?' AND c = ?",
			"SELECT * FROM t WHERE a = $1 AND b = '?' AND c = $2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RebindPostgreSQL(tc.in), "RebindPostgreSQL(%q)", tc.in)
		})
	}
}

func TestEscapeTSQueryTerm(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "invoice", []string{"invoice"}},
		{"unicode_kept", "café", []string{"café"}},
		{"strip_meta_inline", "in&voice|!", []string{"in", "voice"}},
		{"whitespace_splits", "hello world", []string{"hello", "world"}},
		{"all_meta_empty", "&|!():*\\'", nil},
		{"colon_splits", "user:foo", []string{"user", "foo"}},

		// R3 regression cases: previously these leaked punctuation
		// into the tsquery argument and caused to_tsquery to error.
		{"dashes_only", "---", nil},
		{"hyphenated_word", "foo-bar", []string{"foo", "bar"}},
		{"email_address", "user@example.com",
			[]string{"user", "example", "com"}},
		{"dotted_acronym", "a.b.c", []string{"a", "b", "c"}},
		{"mixed_punct", "v1.2.3-rc.1",
			[]string{"v1", "2", "3", "rc", "1"}},
		{"trailing_punct", "invoice---", []string{"invoice"}},
		{"leading_punct", "---invoice", []string{"invoice"}},
		{"digit_only", "12345", []string{"12345"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, EscapeTSQueryTerm(tc.in), "EscapeTSQueryTerm(%q)", tc.in)
		})
	}
}
