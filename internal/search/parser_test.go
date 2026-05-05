package search

import (
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/testutil/ptr"
)

func TestParse(t *testing.T) {
	type testCase struct {
		name  string
		query string
		want  Query
	}

	type testGroup struct {
		name  string
		tests []testCase
	}

	testGroups := []testGroup{
		{
			name: "BasicOperators",
			tests: []testCase{
				{
					name:  "from operator",
					query: "from:alice@example.com",
					want:  Query{FromAddrs: []string{"alice@example.com"}},
				},
				{
					name:  "to operator",
					query: "to:bob@example.com",
					want:  Query{ToAddrs: []string{"bob@example.com"}},
				},
				{
					name:  "multiple from",
					query: "from:alice@example.com from:bob@example.com",
					want:  Query{FromAddrs: []string{"alice@example.com", "bob@example.com"}},
				},
				{
					name:  "bare text",
					query: "hello world",
					want:  Query{TextTerms: []string{"hello", "world"}},
				},
				{
					name:  "quoted phrase",
					query: `"hello world"`,
					want:  Query{TextTerms: []string{"hello world"}},
				},
				{
					name:  "mixed operators and text",
					query: "from:alice@example.com meeting notes",
					want: Query{
						FromAddrs: []string{"alice@example.com"},
						TextTerms: []string{"meeting", "notes"},
					},
				},
			},
		},
		{
			name: "QuotedValues",
			tests: []testCase{
				{
					name:  "subject with quoted phrase",
					query: `subject:"meeting notes"`,
					want:  Query{SubjectTerms: []string{"meeting notes"}},
				},
				{
					name:  "subject with quoted phrase and other terms",
					query: `subject:"project update" from:alice@example.com`,
					want: Query{
						SubjectTerms: []string{"project update"},
						FromAddrs:    []string{"alice@example.com"},
					},
				},
				{
					name:  "label with quoted value containing spaces",
					query: `label:"My Important Label"`,
					want:  Query{Labels: []string{"My Important Label"}},
				},
				{
					name:  "mixed quoted and unquoted",
					query: `subject:urgent subject:"very important" search term`,
					want: Query{
						SubjectTerms: []string{"urgent", "very important"},
						TextTerms:    []string{"search", "term"},
					},
				},
				{
					name:  "from with quoted display name style (edge case)",
					query: `from:"alice@example.com"`,
					want:  Query{FromAddrs: []string{"alice@example.com"}},
				},
			},
		},
		{
			name: "QuotedPhrasesWithColons",
			tests: []testCase{
				{
					name:  "quoted phrase with colon",
					query: `"foo:bar"`,
					want:  Query{TextTerms: []string{"foo:bar"}},
				},
				{
					name:  "quoted phrase with time",
					query: `"meeting at 10:30"`,
					want:  Query{TextTerms: []string{"meeting at 10:30"}},
				},
				{
					name:  "quoted phrase with URL-like content",
					query: `"check http://example.com"`,
					want:  Query{TextTerms: []string{"check http://example.com"}},
				},
				{
					name:  "quoted phrase with multiple colons",
					query: `"a:b:c:d"`,
					want:  Query{TextTerms: []string{"a:b:c:d"}},
				},
				{
					name:  "quoted colon phrase mixed with real operator",
					query: `from:alice@example.com "subject:not an operator"`,
					want: Query{
						FromAddrs: []string{"alice@example.com"},
						TextTerms: []string{"subject:not an operator"},
					},
				},
				{
					name:  "operator followed by quoted colon phrase",
					query: `"re: meeting notes" from:bob@example.com`,
					want: Query{
						TextTerms: []string{"re: meeting notes"},
						FromAddrs: []string{"bob@example.com"},
					},
				},
			},
		},
		{
			name: "Labels",
			tests: []testCase{
				{
					name:  "multiple labels",
					query: "label:INBOX l:work",
					want:  Query{Labels: []string{"INBOX", "work"}},
				},
				{
					name:  "empty label ignored",
					query: "label:",
					want:  Query{},
				},
				{
					name:  "empty label with other terms",
					query: "label: hello",
					want:  Query{TextTerms: []string{"hello"}},
				},
			},
		},
		{
			name: "Subject",
			tests: []testCase{
				{
					name:  "simple subject",
					query: "subject:urgent",
					want:  Query{SubjectTerms: []string{"urgent"}},
				},
			},
		},
		{
			name: "HasAttachment",
			tests: []testCase{
				{
					name:  "has attachment",
					query: "has:attachment",
					want:  Query{HasAttachment: ptr.Bool(true)},
				},
			},
		},
		{
			name: "Dates",
			tests: []testCase{
				{
					name:  "after and before dates",
					query: "after:2024-01-15 before:2024-06-30",
					want: Query{
						AfterDate:  ptr.Time(ptr.Date(2024, 1, 15)),
						BeforeDate: ptr.Time(ptr.Date(2024, 6, 30)),
					},
				},
			},
		},
		{
			name: "Sizes",
			tests: []testCase{
				{
					name:  "larger than 5M",
					query: "larger:5M",
					want:  Query{LargerThan: ptr.Int64(5 * 1024 * 1024)},
				},
				{
					name:  "smaller than 100K",
					query: "smaller:100K",
					want:  Query{SmallerThan: ptr.Int64(100 * 1024)},
				},
				{
					name:  "larger than 1G",
					query: "larger:1G",
					want:  Query{LargerThan: ptr.Int64(1024 * 1024 * 1024)},
				},
			},
		},
		{
			name: "DomainNormalization",
			tests: []testCase{
				{
					name:  "from bare domain gets @ prefix",
					query: "from:example.com",
					want:  Query{FromAddrs: []string{"@example.com"}},
				},
				{
					name:  "from with @ prefix unchanged",
					query: "from:@example.com",
					want:  Query{FromAddrs: []string{"@example.com"}},
				},
				{
					name:  "from full email unchanged",
					query: "from:alice@example.com",
					want:  Query{FromAddrs: []string{"alice@example.com"}},
				},
				{
					name:  "to bare domain gets @ prefix",
					query: "to:example.com",
					want:  Query{ToAddrs: []string{"@example.com"}},
				},
				{
					name:  "from bare word without dot unchanged",
					query: "from:alice",
					want:  Query{FromAddrs: []string{"alice"}},
				},
				{
					name:  "from subdomain gets @ prefix",
					query: "from:mail.example.co.uk",
					want:  Query{FromAddrs: []string{"@mail.example.co.uk"}},
				},
				{
					name:  "from dotted local part unchanged",
					query: "from:john.doe",
					want:  Query{FromAddrs: []string{"john.doe"}},
				},
				{
					name:  "dotted local part with three segments unchanged",
					query: "to:first.middle.last",
					want:  Query{ToAddrs: []string{"first.middle.last"}},
				},
				{
					name:  "from two-letter ccTLD detected as domain",
					query: "from:company.io",
					want:  Query{FromAddrs: []string{"@company.io"}},
				},
				{
					name:  "new gTLD email detected as domain",
					query: "from:contact.email",
					want:  Query{FromAddrs: []string{"@contact.email"}},
				},
				{
					name:  "new gTLD news detected as domain",
					query: "from:brand.news",
					want:  Query{FromAddrs: []string{"@brand.news"}},
				},
			},
		},
		{
			name: "ComplexQuery",
			tests: []testCase{
				{
					name:  "complex query",
					query: `from:alice@example.com to:bob@example.com subject:meeting has:attachment after:2024-01-01 "project report"`,
					want: Query{
						FromAddrs:     []string{"alice@example.com"},
						ToAddrs:       []string{"bob@example.com"},
						SubjectTerms:  []string{"meeting"},
						TextTerms:     []string{"project report"},
						HasAttachment: ptr.Bool(true),
						AfterDate:     ptr.Time(ptr.Date(2024, 1, 1)),
					},
				},
			},
		},
	}

	for _, group := range testGroups {
		t.Run(group.name, func(t *testing.T) {
			for _, tt := range group.tests {
				t.Run(tt.name, func(t *testing.T) {
					got := Parse(tt.query)
					assertQueryEqual(t, *got, tt.want)
				})
			}
		})
	}
}

func TestParse_RelativeDates(t *testing.T) {
	fixedNow := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)
	p := &Parser{Now: func() time.Time { return fixedNow }}

	tests := []struct {
		name  string
		query string
		want  Query
	}{
		{
			name:  "newer_than days",
			query: "newer_than:7d",
			want:  Query{AfterDate: ptr.Time(ptr.Date(2025, 6, 8))},
		},
		{
			name:  "older_than weeks",
			query: "older_than:2w",
			want:  Query{BeforeDate: ptr.Time(ptr.Date(2025, 6, 1))},
		},
		{
			name:  "newer_than months",
			query: "newer_than:1m",
			want:  Query{AfterDate: ptr.Time(ptr.Date(2025, 5, 15))},
		},
		{
			name:  "older_than years",
			query: "older_than:1y",
			want:  Query{BeforeDate: ptr.Time(ptr.Date(2024, 6, 15))},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Parse(tt.query)
			assertQueryEqual(t, *got, tt.want)
		})
	}
}

// TestParse_TopLevelWrapper ensures the convenience Parse() function
// works correctly with relative date operators (verifies wiring to NewParser).
func TestParse_TopLevelWrapper(t *testing.T) {
	// Test that Parse() handles relative dates without panicking
	// and returns a non-nil AfterDate (the exact value depends on current time)
	q := Parse("newer_than:1d")
	if q.AfterDate == nil {
		t.Error("Parse(\"newer_than:1d\") should set AfterDate")
	}

	// Also verify older_than sets BeforeDate
	q = Parse("older_than:1w")
	if q.BeforeDate == nil {
		t.Error("Parse(\"older_than:1w\") should set BeforeDate")
	}
}

// TestParser_NilNow verifies that a Parser with nil Now function doesn't panic
// and correctly handles relative date operators by falling back to time.Now().
func TestParser_NilNow(t *testing.T) {
	p := &Parser{Now: nil}

	// Should not panic and should return a valid result
	q := p.Parse("newer_than:1d")
	if q.AfterDate == nil {
		t.Fatal("Parser{Now: nil}.Parse(\"newer_than:1d\") should set AfterDate")
	}

	now := time.Now().UTC()
	// AfterDate should be within a tight window around now-24h
	// Allow some tolerance for test execution time: between now-36h and now-12h
	earliestExpected := now.Add(-36 * time.Hour)
	latestExpected := now.Add(-12 * time.Hour)

	if q.AfterDate.Before(earliestExpected) {
		t.Errorf("AfterDate %v is too far in the past (expected after %v)", q.AfterDate, earliestExpected)
	}
	if q.AfterDate.After(latestExpected) {
		t.Errorf("AfterDate %v is too recent (expected before %v)", q.AfterDate, latestExpected)
	}
	if q.AfterDate.After(now) {
		t.Errorf("AfterDate %v is in the future", q.AfterDate)
	}
}

func TestQuery_IsEmpty(t *testing.T) {
	tests := []struct {
		query   string
		isEmpty bool
	}{
		{"", true},
		{"from:alice@example.com", false},
		{"hello", false},
		{"has:attachment", false},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			q := Parse(tt.query)
			if q.IsEmpty() != tt.isEmpty {
				t.Errorf("IsEmpty(%q): got %v, want %v", tt.query, q.IsEmpty(), tt.isEmpty)
			}
		})
	}

	t.Run("AccountIDs only", func(t *testing.T) {
		q := &Query{}
		id := int64(42)
		q.AccountIDs = []int64{id}
		if q.IsEmpty() {
			t.Error("IsEmpty() = true for query with AccountIDs set")
		}
	})
}
