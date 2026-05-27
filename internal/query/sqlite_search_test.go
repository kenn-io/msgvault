package query

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/ptr"
)

func TestSearch_Filters(t *testing.T) {
	after := ptr.Date(2024, 2, 1)
	before := ptr.Date(2024, 3, 1)
	largerThan := int64(2500)

	tests := []struct {
		name      string
		query     *search.Query
		wantCount int
		validator func(MessageSummary) bool
		validDesc string
	}{
		{
			name:      "WithoutFTS",
			query:     &search.Query{TextTerms: []string{"Hello"}},
			wantCount: 2,
		},
		{
			name:      "FromFilter",
			query:     &search.Query{FromAddrs: []string{"alice@example.com"}},
			wantCount: 3,
			validator: func(m MessageSummary) bool { return m.FromEmail == "alice@example.com" },
			validDesc: "FromEmail=alice@example.com",
		},
		{
			name:      "LabelFilter",
			query:     &search.Query{Labels: []string{"Work"}},
			wantCount: 2,
		},
		{
			name:      "LabelFilter_CaseInsensitive",
			query:     &search.Query{Labels: []string{"work"}},
			wantCount: 2,
		},
		{
			name:      "LabelFilter_Substring",
			query:     &search.Query{Labels: []string{"wor"}},
			wantCount: 2, // matches "Work"
		},
		{
			name:      "DateRangeFilter",
			query:     &search.Query{AfterDate: &after, BeforeDate: &before},
			wantCount: 2,
		},
		{
			name:      "HasAttachment",
			query:     &search.Query{HasAttachment: new(true)},
			wantCount: 2,
			validator: func(m MessageSummary) bool { return m.HasAttachments },
			validDesc: "HasAttachments=true",
		},
		{
			name:      "CombinedFilters",
			query:     &search.Query{FromAddrs: []string{"alice@example.com"}, Labels: []string{"Work"}},
			wantCount: 1,
		},
		{
			name:      "SizeFilter",
			query:     &search.Query{LargerThan: new(largerThan)},
			wantCount: 1,
			validator: func(m MessageSummary) bool { return m.SizeEstimate > largerThan },
			validDesc: "SizeEstimate>2500",
		},
		{
			name:      "EmptyQuery",
			query:     &search.Query{},
			wantCount: 5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			results := assertSearchCount(t, env, tc.query, tc.wantCount)
			if tc.validator != nil {
				assertAllResults(t, results, tc.validDesc, tc.validator)
			}
		})
	}
}

func TestSearch_CaseInsensitiveFallback(t *testing.T) {
	env := newTestEnv(t)

	if env.Engine.hasFTSTable(env.Ctx) {
		t.Skip("FTS is available; this test covers the non-FTS fallback path")
	}

	q := &search.Query{TextTerms: []string{"hello"}}
	assertSearchCount(t, env, q, 2)

	q = &search.Query{TextTerms: []string{"WORLD"}}
	results := assertSearchCount(t, env, q, 1)

	if len(results) > 0 {
		assertpkg.Equal(t, "Hello World", results[0].Subject)
	}
}

func TestSearch_SubjectTermsCaseInsensitive(t *testing.T) {
	env := newTestEnv(t)

	if env.Engine.hasFTSTable(env.Ctx) {
		t.Skip("FTS is available; this test covers the non-FTS fallback path")
	}

	q := &search.Query{SubjectTerms: []string{"HELLO"}}
	assertSearchCount(t, env, q, 2)
}

func TestSearch_WithFTS(t *testing.T) {
	env := newTestEnv(t)
	env.EnableFTS()

	q := &search.Query{TextTerms: []string{"World"}}
	results := assertSearchCount(t, env, q, 1)

	requirepkg.NotEmpty(t, results)
	assertpkg.Equal(t, "Hello World", results[0].Subject)
}

// TestSearch_WithFTS_SpecialChars verifies that FTS5 special characters in
// search terms don't cause syntax errors. Without quoting, these characters
// are interpreted as FTS5 operators (- = NOT, : = column filter, () = grouping).
func TestSearch_WithFTS_SpecialChars(t *testing.T) {
	env := newTestEnv(t)
	env.EnableFTS()

	tests := []struct {
		name string
		term string
	}{
		{"colon", "Re:"},
		{"hyphen", "foo-bar"},
		{"parens", "(test)"},
		{"mixed_special", "re:hello-world"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := &search.Query{TextTerms: []string{tc.term}}
			_, err := env.Engine.Search(env.Ctx, q, 100, 0)
			assertpkg.NoError(t, err, "FTS5 search for %q should not error", tc.term)
		})
	}
}

func TestHasFTSTable(t *testing.T) {
	env := newTestEnv(t)

	assertpkg.False(t, env.Engine.hasFTSTable(env.Ctx),
		"expected hasFTSTable to return false for test DB without FTS")

	_, err := env.DB.Exec(`
		CREATE VIRTUAL TABLE messages_fts USING fts5(subject, snippet);
	`)
	if err != nil {
		t.Skipf("FTS5 not available in this SQLite build: %v", err)
	}

	engine2 := NewSQLiteEngine(env.DB)

	assertpkg.True(t, engine2.hasFTSTable(env.Ctx),
		"expected hasFTSTable to return true after creating FTS table")
}

func TestHasFTSTable_ErrorDoesNotCache(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.DB.Exec(`
		CREATE VIRTUAL TABLE messages_fts USING fts5(subject, snippet);
	`)
	if err != nil {
		t.Skipf("FTS5 not available, cannot verify error-does-not-cache behavior: %v", err)
	}

	env.Engine = NewSQLiteEngine(env.DB)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	firstResult := env.Engine.hasFTSTable(canceledCtx)

	if firstResult {
		t.Skip("SQLite driver does not error on canceled context; cannot test error-retry behavior")
	}

	validCtx := context.Background()
	secondResult := env.Engine.hasFTSTable(validCtx)

	assertpkg.True(t, secondResult,
		"hasFTSTable retry returned false, but FTS is available; error was incorrectly cached")

	thirdResult := env.Engine.hasFTSTable(validCtx)
	assertpkg.True(t, thirdResult, "hasFTSTable cached result is false, expected true")
}

func TestSearchWithDomainFilter(t *testing.T) {
	env := newTestEnv(t)

	q := &search.Query{FromAddrs: []string{"@example.com"}}
	results, err := env.Engine.Search(env.Ctx, q, 1000, 0)
	requirepkg.NoError(t, err, "Search")
	requirepkg.GreaterOrEqual(t, len(results), 3, "expected at least 3 results")
	assertAllResults(t, results, "FromEmail ends with @example.com", func(m MessageSummary) bool {
		return m.FromEmail == "" || strings.HasSuffix(m.FromEmail, "@example.com")
	})
}

func TestSearchMixedExactAndDomainFilter(t *testing.T) {
	env := newTestEnv(t)

	q := &search.Query{FromAddrs: []string{"alice@example.com", "@other.com"}}
	results := env.MustSearch(q, 100, 0)

	requirepkg.NotEmpty(t, results, "Expected at least one result")
	assertAllResults(t, results, "FromEmail matches alice@example.com or @other.com", func(m MessageSummary) bool {
		return m.FromEmail == "alice@example.com" || strings.HasSuffix(m.FromEmail, "@other.com")
	})
	assertAnyResult(t, results, "FromEmail equals alice@example.com", func(m MessageSummary) bool {
		return m.FromEmail == "alice@example.com"
	})
}

// TestSearchFastCountMatchesSearch verifies that SearchFastCount returns the same
// count as the number of results from Search for various query types.
func TestSearchFastCountMatchesSearch(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name  string
		query *search.Query
	}{
		{
			name:  "from filter",
			query: &search.Query{FromAddrs: []string{"alice@example.com"}},
		},
		{
			name:  "to filter",
			query: &search.Query{ToAddrs: []string{"bob@example.com"}},
		},
		{
			name:  "label filter",
			query: &search.Query{Labels: []string{"INBOX"}},
		},
		{
			name:  "subject filter",
			query: &search.Query{SubjectTerms: []string{"Test"}},
		},
		{
			name:  "combined filters",
			query: &search.Query{FromAddrs: []string{"alice@example.com"}, Labels: []string{"INBOX"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results, err := env.Engine.Search(env.Ctx, tc.query, 1000, 0)
			requirepkg.NoError(t, err, "Search")

			count, err := env.Engine.SearchFastCount(env.Ctx, tc.query, MessageFilter{})
			requirepkg.NoError(t, err, "SearchFastCount")

			assertpkg.Equal(t, int64(len(results)), count, "SearchFastCount mismatch")
		})
	}
}

// =============================================================================
// MergeFilterIntoQuery tests
// =============================================================================

func TestMergeFilterIntoQuery(t *testing.T) {
	sourceID42 := int64(42)
	sourceID1 := int64(1)

	tests := []struct {
		name     string
		initial  *search.Query
		filter   MessageFilter
		expected *search.Query
	}{
		{
			name: "EmptyFilter",
			initial: &search.Query{
				TextTerms: []string{"test", "query"},
				FromAddrs: []string{"alice@example.com"},
				Labels:    []string{"inbox"},
			},
			filter: MessageFilter{},
			expected: &search.Query{
				TextTerms: []string{"test", "query"},
				FromAddrs: []string{"alice@example.com"},
				Labels:    []string{"inbox"},
			},
		},
		{
			name:     "SourceID",
			initial:  &search.Query{},
			filter:   MessageFilter{SourceID: &sourceID42},
			expected: &search.Query{AccountIDs: []int64{sourceID42}},
		},
		{
			name:     "SenderAppends",
			initial:  &search.Query{FromAddrs: []string{"alice@example.com"}},
			filter:   MessageFilter{Sender: "bob@example.com"},
			expected: &search.Query{FromAddrs: []string{"alice@example.com", "bob@example.com"}},
		},
		{
			name:     "RecipientAppends",
			initial:  &search.Query{ToAddrs: []string{"recipient1@example.com"}},
			filter:   MessageFilter{Recipient: "recipient2@example.com"},
			expected: &search.Query{ToAddrs: []string{"recipient1@example.com", "recipient2@example.com"}},
		},
		{
			name:     "LabelAppends",
			initial:  &search.Query{Labels: []string{"inbox"}},
			filter:   MessageFilter{Label: "important"},
			expected: &search.Query{Labels: []string{"inbox", "important"}},
		},
		{
			name:     "Attachments",
			initial:  &search.Query{},
			filter:   MessageFilter{WithAttachmentsOnly: true},
			expected: &search.Query{HasAttachment: new(true)},
		},
		{
			name:     "Domain",
			initial:  &search.Query{},
			filter:   MessageFilter{Domain: "example.com"},
			expected: &search.Query{FromAddrs: []string{"@example.com"}},
		},
		{
			name: "MultipleFilters",
			initial: &search.Query{
				TextTerms: []string{"search", "term"},
				FromAddrs: []string{"alice@example.com"},
			},
			filter: MessageFilter{
				SourceID:            &sourceID1,
				Sender:              "bob@example.com",
				Recipient:           "carol@example.com",
				Label:               "starred",
				WithAttachmentsOnly: true,
				Domain:              "domain.com",
			},
			expected: &search.Query{
				TextTerms:     []string{"search", "term"},
				FromAddrs:     []string{"alice@example.com", "bob@example.com", "@domain.com"},
				ToAddrs:       []string{"carol@example.com"},
				Labels:        []string{"starred"},
				HasAttachment: new(true),
				AccountIDs:    []int64{sourceID1},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			merged := MergeFilterIntoQuery(tc.initial, tc.filter)
			diff := cmp.Diff(tc.expected, merged, cmpopts.EquateEmpty())
			assertpkg.Empty(t, diff, "MergeFilterIntoQuery mismatch (-want +got):\n%s", diff)
		})
	}
}

func TestMergeFilterIntoQuery_DoesNotMutateOriginal(t *testing.T) {
	q := &search.Query{FromAddrs: []string{"original@example.com"}}
	filter := MessageFilter{Sender: "added@example.com"}

	_ = MergeFilterIntoQuery(q, filter)

	requirepkg.Len(t, q.FromAddrs, 1, "Original query was mutated")
	assertpkg.Equal(t, "original@example.com", q.FromAddrs[0], "Original query was mutated")
}

// TestMergeFilterIntoQuery_EmptySourceIDsClearsAccountScope verifies that
// an explicit empty (non-nil) SourceIDs slice is treated as match-nothing,
// matching appendSourceFilter's contract. Previously the code only
// applied SourceIDs when len > 0, so an explicit empty slice silently
// fell through and let the original query's AccountIDs leak through.
func TestMergeFilterIntoQuery_EmptySourceIDsClearsAccountScope(t *testing.T) {
	q := &search.Query{AccountIDs: []int64{1, 2, 3}}
	filter := MessageFilter{SourceIDs: []int64{}} // non-nil, len=0

	merged := MergeFilterIntoQuery(q, filter)
	requirepkg.NotNil(t, merged.AccountIDs, "want non-nil empty slice (match-nothing)")
	assertpkg.Empty(t, merged.AccountIDs, "want empty (match-nothing)")
}

// TestMergeFilterIntoQuery_NilSourceIDsPreservesAccountScope verifies the
// flip-side: a nil SourceIDs slice is "no override", and the original
// query's AccountIDs survive unchanged.
func TestMergeFilterIntoQuery_NilSourceIDsPreservesAccountScope(t *testing.T) {
	q := &search.Query{AccountIDs: []int64{1, 2, 3}}
	filter := MessageFilter{} // SourceIDs is nil

	merged := MergeFilterIntoQuery(q, filter)
	assertpkg.Len(t, merged.AccountIDs, 3, "want [1 2 3]")
}

func TestMergeFilterIntoQuery_SliceAliasingMutation(t *testing.T) {
	backing := make([]string, 1, 10)
	backing[0] = "original@example.com"

	q := &search.Query{FromAddrs: backing[:1]}
	filter := MessageFilter{Sender: "added@example.com"}

	merged := MergeFilterIntoQuery(q, filter)

	requirepkg.Len(t, merged.FromAddrs, 2)

	requirepkg.Len(t, q.FromAddrs, 1, "Original query was mutated via slice aliasing")
	assertpkg.Equal(t, "original@example.com", q.FromAddrs[0], "Original FromAddrs[0] was changed")
}

// TestSearchByDomains_HidesDeleted verifies SearchByDomains applies the
// same live-message filter as Search/SearchFast: dedup losers (deleted_at)
// are always hidden, and source-deleted rows (deleted_from_source_at) are
// hidden too. Without the predicate this MCP-facing surface would surface
// rows that every other read path suppresses.
func TestSearchByDomains_HidesDeleted(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	ctx := context.Background()

	all, err := env.Engine.SearchByDomains(ctx, []string{"example.com"}, nil, nil, 100, 0)
	require.NoError(err, "SearchByDomains baseline")
	require.Len(all, 5, "baseline result count")

	// Dedup-soft-delete one message — must always be hidden.
	env.MarkDedupLoserByID(1)
	// Source-delete another — also hidden by the full live-message predicate.
	env.MarkDeletedByID(2)

	results, err := env.Engine.SearchByDomains(ctx, []string{"example.com"}, nil, nil, 100, 0)
	require.NoError(err, "SearchByDomains after deletes")
	assert.Len(results, 3, "after deletes")
	for _, r := range results {
		assert.NotEqual(int64(1), r.ID, "dedup-loser message 1 leaked into results")
		assert.NotEqual(int64(2), r.ID, "source-deleted message 2 leaked into results")
	}
}

func TestSearch_HideDeleted(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Search without HideDeleted: all messages returned
	q := &search.Query{}
	all := env.MustSearch(q, 100, 0)
	assert.Len(all, 5, "Search without HideDeleted")

	// Search with HideDeleted: deleted message excluded
	q = &search.Query{HideDeleted: true}
	filtered := env.MustSearch(q, 100, 0)
	assert.Len(filtered, 4, "Search with HideDeleted")

	// MergeFilterIntoQuery carries HideDeletedFromSource → HideDeleted
	baseQ := &search.Query{}
	filter := MessageFilter{HideDeletedFromSource: true}
	merged := MergeFilterIntoQuery(baseQ, filter)
	assert.True(merged.HideDeleted,
		"MergeFilterIntoQuery should set HideDeleted from HideDeletedFromSource")
	results := env.MustSearch(merged, 100, 0)
	assert.Len(results, 4, "Search via merged query")
}
