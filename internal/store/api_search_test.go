package store_test

import (
	"database/sql"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestSearchMessagesQuery_TokenlessTextTerms verifies that text terms which
// reduce to nothing usable under the FTS tokenizer ("!!!", "---", "")
// neither error nor short-circuit through the FTS function. PG's
// to_tsquery('simple', ”) raises "text-search query doesn't contain
// lexemes" and SQLite's FTS5 MATCH on an empty/punctuation-only string is
// a syntax error; the store now substitutes a FALSE condition so the
// query returns zero rows from any backend without ever building a
// malformed FTS argument. Runs under both SQLite and PostgreSQL.
func TestSearchMessagesQuery_TokenlessTextTerms(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)

	// Seed two messages with real searchable content so the test would
	// see a non-zero baseline if the FTS predicate were dropped instead
	// of replaced with FALSE.
	msg1 := f.NewMessage().
		WithSourceMessageID("search-msg-1").
		WithSubject("invoice attached").
		WithSnippet("see the attached invoice").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(msg1,
		sql.NullString{String: "invoice body text", Valid: true},
		sql.NullString{}), "UpsertMessageBody 1")

	msg2 := f.NewMessage().
		WithSourceMessageID("search-msg-2").
		WithSubject("project update").
		WithSnippet("weekly status").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(msg2,
		sql.NullString{String: "project body text", Valid: true},
		sql.NullString{}), "UpsertMessageBody 2")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	// Sanity: a real term must still match — proves the test setup is
	// wired correctly and isn't accidentally returning zero for everything.
	msgs, total, err := f.Store.SearchMessagesQuery(
		&search.Query{TextTerms: []string{"invoice"}}, 0, 50,
	)
	require.NoError(err, "baseline search")
	require.GreaterOrEqual(total, int64(1), "baseline search 'invoice' returned %d hits", total)
	require.GreaterOrEqual(len(msgs), 1)

	// Each of these reduces to no usable tokens. Must not error and
	// must return zero rows (FALSE predicate substituted by the caller).
	cases := []struct {
		name  string
		terms []string
	}{
		{"only_punctuation", []string{"!!!"}},
		{"only_dashes", []string{"---"}},
		{"empty_string", []string{""}},
		{"mixed_all_empty", []string{"!!!", "---", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, total, err := f.Store.SearchMessagesQuery(
				&search.Query{TextTerms: tc.terms}, 0, 50,
			)
			requirepkg.NoError(t, err, "SearchMessagesQuery(%v)", tc.terms)
			assertpkg.Equal(t, int64(0), total, "total (FALSE predicate should match nothing)")
			assertpkg.Empty(t, msgs)
		})
	}
}

// TestSearchMessages_LegacyRawString verifies the legacy SearchMessages
// entrypoint (raw-string FTS query) sanitizes its input through the
// dialect's BuildFTSArg pipeline. Previously it bound the raw string
// straight into FTSSearchClause's placeholder, so any whitespace or
// metacharacter in a user search would reach to_tsquery on PG (parser
// error) or FTS5 MATCH on SQLite (syntax error). Routing through
// SearchMessagesQuery shares the same FALSE fallback as
// TokenlessTextTerms and lets multi-word queries actually work.
func TestSearchMessages_LegacyRawString(t *testing.T) {
	f := storetest.New(t)

	msg1 := f.NewMessage().
		WithSourceMessageID("legacy-msg-1").
		WithSubject("urgent invoice").
		WithSnippet("please review").
		Create(t, f.Store)
	requirepkg.NoError(t, f.Store.UpsertMessageBody(msg1,
		sql.NullString{String: "invoice body for review", Valid: true},
		sql.NullString{}), "UpsertMessageBody 1")

	msg2 := f.NewMessage().
		WithSourceMessageID("legacy-msg-2").
		WithSubject("project plan").
		WithSnippet("status update").
		Create(t, f.Store)
	requirepkg.NoError(t, f.Store.UpsertMessageBody(msg2,
		sql.NullString{String: "project plan body", Valid: true},
		sql.NullString{}), "UpsertMessageBody 2")

	_, err := f.Store.BackfillFTS(nil)
	requirepkg.NoError(t, err, "BackfillFTS")

	// Multi-word query was the canonical PG failure: "invoice review"
	// fed straight into to_tsquery would error. Now it tokenizes into
	// two terms AND'd by the dialect helper.
	t.Run("multi_word_match", func(t *testing.T) {
		msgs, total, err := f.Store.SearchMessages("invoice review", 0, 50)
		requirepkg.NoError(t, err, "SearchMessages('invoice review')")
		requirepkg.GreaterOrEqual(t, total, int64(1), "expected >= 1 hit for 'invoice review'")
		requirepkg.GreaterOrEqual(t, len(msgs), 1)
	})

	// Single-word query still works.
	t.Run("single_word_match", func(t *testing.T) {
		msgs, total, err := f.Store.SearchMessages("project", 0, 50)
		requirepkg.NoError(t, err, "SearchMessages('project')")
		requirepkg.GreaterOrEqual(t, total, int64(1), "expected >= 1 hit for 'project'")
		requirepkg.GreaterOrEqual(t, len(msgs), 1)
	})

	// Each of these reduces to no usable tokens after splitting on
	// whitespace and per-dialect sanitization. Must not error.
	cases := []struct {
		name  string
		query string
	}{
		{"only_punctuation", "!!!"},
		{"only_dashes", "---"},
		{"whitespace_only", "   \t  "},
		{"mixed_punctuation", "!!! --- ???"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, total, err := f.Store.SearchMessages(tc.query, 0, 50)
			requirepkg.NoError(t, err, "SearchMessages(%q)", tc.query)
			assertpkg.Equal(t, int64(0), total)
			assertpkg.Empty(t, msgs)
		})
	}
}
