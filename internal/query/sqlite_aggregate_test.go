package query

import (
	"context"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestAggregations(t *testing.T) {
	type testCase struct {
		name    string
		aggName string
		view    ViewType
		want    []aggExpectation
	}

	tests := []testCase{
		{
			name:    "BySender",
			aggName: "AggregateBySender",
			view:    ViewSenders,
			want:    []aggExpectation{{"alice@example.com", 3}, {"bob@company.org", 2}},
		},
		{
			name:    "BySenderName",
			aggName: "AggregateBySenderName",
			view:    ViewSenderNames,
			want:    []aggExpectation{{"Alice Smith", 3}, {"Bob Jones", 2}},
		},
		{
			name:    "ByRecipient",
			aggName: "AggregateByRecipient",
			view:    ViewRecipients,
			want:    []aggExpectation{{"bob@company.org", 3}, {"alice@example.com", 2}, {"carol@example.com", 1}},
		},
		{
			name:    "ByDomain",
			aggName: "AggregateByDomain",
			view:    ViewDomains,
			want:    []aggExpectation{{"example.com", 3}, {"company.org", 2}},
		},
		{
			name:    "ByLabel",
			aggName: "AggregateByLabel",
			view:    ViewLabels,
			want:    []aggExpectation{{"INBOX", 5}, {"Work", 2}, {"IMPORTANT", 1}},
		},
		{
			name:    "ByRecipientName",
			aggName: "AggregateByRecipientName",
			view:    ViewRecipientNames,
			want:    []aggExpectation{{"Bob Jones", 3}, {"Alice Smith", 2}, {"Carol White", 1}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			rows, err := env.Engine.Aggregate(env.Ctx, tc.view, DefaultAggregateOptions())
			requirepkg.NoError(t, err, tc.aggName)
			assertAggRows(t, rows, tc.want)
		})
	}
}

func TestAggregateBySenderName_FallbackToEmail(t *testing.T) {
	env := newTestEnv(t)

	noNameID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("noname@test.com"), DisplayName: nil, Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "No Name Test", SentAt: "2024-05-01 10:00:00", FromID: noNameID})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenderNames, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "AggregateBySenderName")

	assertpkg.Len(t, rows, 3, "expected 3 sender names")

	assertRow(t, rows, "noname@test.com")
}

// TestAggregateBySenderName_FallbackToPhone covers phone-only iMessage/SMS
// participants without a contacts entry: display_name and email_address are
// both empty/NULL but phone_number is set. They must surface under the phone
// number, not vanish from the SenderNames aggregate.
func TestAggregateBySenderName_FallbackToPhone(t *testing.T) {
	env := newTestEnv(t)

	phoneOnlyID := env.AddParticipant(dbtest.ParticipantOpts{
		Phone:       new("+15551234567"),
		DisplayName: new(""),
	})
	env.AddMessage(dbtest.MessageOpts{Subject: "SMS", SentAt: "2024-05-01 10:00:00", FromID: phoneOnlyID})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenderNames, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "AggregateBySenderName")

	assertRow(t, rows, "+15551234567")

	// Same fallback drives the SenderName filter.
	listed := env.MustListMessages(MessageFilter{SenderName: "+15551234567"})
	assertpkg.Len(t, listed, 1, "ListMessages by phone-fallback name")
}

// TestAggregateByRecipientName_FallbackToPhone is the recipient-side analog.
func TestAggregateByRecipientName_FallbackToPhone(t *testing.T) {
	env := newTestEnv(t)

	phoneOnlyID := env.AddParticipant(dbtest.ParticipantOpts{
		Phone:       new("+15557654321"),
		DisplayName: new(""),
	})
	senderID := env.MustLookupParticipant("alice@example.com")
	env.AddMessage(dbtest.MessageOpts{
		Subject: "Group", SentAt: "2024-05-01 10:00:00",
		FromID: senderID, ToIDs: []int64{phoneOnlyID},
	})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "AggregateByRecipientName")

	assertRow(t, rows, "+15557654321")

	listed := env.MustListMessages(MessageFilter{RecipientName: "+15557654321"})
	assertpkg.Len(t, listed, 1, "ListMessages by phone-fallback recipient name")
}

func TestAggregateBySenderName_EmptyStringFallback(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	emptyID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("empty@test.com"), DisplayName: new(""), Domain: "test.com"})
	spacesID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("spaces@test.com"), DisplayName: new("   "), Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "Empty Name", SentAt: "2024-05-01 10:00:00", FromID: emptyID})
	env.AddMessage(dbtest.MessageOpts{Subject: "Spaces Name", SentAt: "2024-05-02 10:00:00", FromID: spacesID})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenderNames, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "AggregateBySenderName")

	if !assert.Len(rows, 4, "expected 4 sender names") {
		for _, r := range rows {
			t.Logf("  key=%q count=%d", r.Key, r.Count)
		}
	}

	for _, r := range rows {
		assert.NotEmpty(r.Key, "unexpected empty key")
		assert.NotEqual("   ", r.Key, "unexpected whitespace key")
	}
	assertRowsContain(t, rows, []aggExpectation{
		{"empty@test.com", 1},
		{"spaces@test.com", 1},
	})
}

func TestAggregateByTime(t *testing.T) {
	env := newTestEnv(t)

	opts := DefaultAggregateOptions()
	opts.TimeGranularity = TimeMonth

	rows, err := env.Engine.Aggregate(env.Ctx, ViewTime, opts)
	requirepkg.NoError(t, err, "AggregateByTime")

	assertAggRows(t, rows, []aggExpectation{
		{"2024-01", 2},
		{"2024-02", 2},
		{"2024-03", 1},
	})
}

func TestAggregateWithDateFilter(t *testing.T) {
	env := newTestEnv(t)

	opts := DefaultAggregateOptions()
	after := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	opts.After = &after

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	requirepkg.NoError(t, err, "AggregateBySender with date filter")

	assertAggRows(t, rows, []aggExpectation{
		{"bob@company.org", 2},
		{"alice@example.com", 1},
	})
}

func TestSortingOptions(t *testing.T) {
	env := newTestEnv(t)

	t.Run("SortBySizeDesc", func(t *testing.T) {
		opts := DefaultAggregateOptions()
		opts.SortField = SortBySize
		rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
		requirepkg.NoError(t, err, "AggregateBySender")
		assertAggRows(t, rows, []aggExpectation{
			{"alice@example.com", 3},
			{"bob@company.org", 2},
		})
	})

	t.Run("SortBySizeAsc", func(t *testing.T) {
		opts := DefaultAggregateOptions()
		opts.SortField = SortBySize
		opts.SortDirection = SortAsc
		rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
		requirepkg.NoError(t, err, "AggregateBySender")
		assertAggRows(t, rows, []aggExpectation{
			{"bob@company.org", 2},
			{"alice@example.com", 3},
		})
	})
}

func TestWithAttachmentsOnlyAggregate(t *testing.T) {
	env := newTestEnv(t)

	opts := DefaultAggregateOptions()
	allRows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	requirepkg.NoError(t, err, "AggregateBySender")

	assertRowsContain(t, allRows, []aggExpectation{
		{"alice@example.com", 3},
		{"bob@company.org", 2},
	})

	opts.WithAttachmentsOnly = true
	attRows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	requirepkg.NoError(t, err, "AggregateBySender with attachment filter")

	assertRowsContain(t, attRows, []aggExpectation{
		{"alice@example.com", 1},
		{"bob@company.org", 1},
	})
}

// =============================================================================
// SubAggregate tests
// =============================================================================

func TestSubAggregates(t *testing.T) {
	tests := []struct {
		name   string
		filter MessageFilter
		view   ViewType
		want   []aggExpectation
	}{
		{
			name:   "BySender",
			filter: MessageFilter{Recipient: "alice@example.com"},
			view:   ViewSenders,
			want:   []aggExpectation{{"bob@company.org", 2}},
		},
		{
			name:   "BySenderName",
			filter: MessageFilter{Recipient: "alice@example.com"},
			view:   ViewSenderNames,
			want:   []aggExpectation{{"Bob Jones", 2}},
		},
		{
			name:   "ByRecipient",
			filter: MessageFilter{Sender: "alice@example.com"},
			view:   ViewRecipients,
			want:   []aggExpectation{{"bob@company.org", 3}, {"carol@example.com", 1}},
		},
		{
			name:   "ByLabel",
			filter: MessageFilter{Sender: "alice@example.com"},
			view:   ViewLabels,
			want:   []aggExpectation{{"INBOX", 3}, {"IMPORTANT", 1}, {"Work", 1}},
		},
		{
			name:   "ByRecipientName",
			filter: MessageFilter{Sender: "alice@example.com"},
			view:   ViewRecipientNames,
			want:   []aggExpectation{{"Bob Jones", 3}, {"Carol White", 1}},
		},
		{
			name:   "RecipientNameWithRecipient",
			filter: MessageFilter{Recipient: "bob@company.org", RecipientName: "Bob Jones"},
			view:   ViewSenders,
			want:   []aggExpectation{{"alice@example.com", 3}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			results, err := env.Engine.SubAggregate(env.Ctx, tc.filter, tc.view, DefaultAggregateOptions())
			requirepkg.NoError(t, err, "SubAggregate")
			assertAggRows(t, results, tc.want)
		})
	}
}

func TestSubAggregate_MatchEmptySenderName(t *testing.T) {
	env := newTestEnvWithEmptyBuckets(t)

	filter := MessageFilter{EmptyValueTargets: map[ViewType]bool{ViewSenderNames: true}}
	results, err := env.Engine.SubAggregate(env.Ctx, filter, ViewLabels, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "SubAggregate with MatchEmptySenderName")

	if !assertpkg.Empty(t, results, "expected 0 label sub-aggregates for empty sender name") {
		for _, r := range results {
			t.Logf("  key=%q count=%d", r.Key, r.Count)
		}
	}
}

func TestSubAggregateIncludesDeletedMessages(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{Sender: "alice@example.com"}
	resultsBefore, err := env.Engine.SubAggregate(env.Ctx, filter, ViewRecipients, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "SubAggregate before")

	env.MarkDeletedByID(1)

	resultsAfter, err := env.Engine.SubAggregate(env.Ctx, filter, ViewRecipients, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "SubAggregate after")

	var totalBefore, totalAfter int64
	for _, r := range resultsBefore {
		totalBefore += r.Count
	}
	for _, r := range resultsAfter {
		totalAfter += r.Count
	}

	assertpkg.Equal(t, totalBefore, totalAfter,
		"expected same message count (deleted included)")
}

func TestHideDeletedFromSourceAggregate(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	// Before deletion: all 5 messages visible
	opts := DefaultAggregateOptions()
	allRows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	require.NoError(err, "Aggregate")
	var totalBefore int64
	for _, r := range allRows {
		totalBefore += r.Count
	}
	require.Equal(int64(5), totalBefore, "expected 5 total messages before deletion")

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Without HideDeletedFromSource: deleted messages still included
	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	require.NoError(err, "Aggregate (no hide)")
	var totalWithDeleted int64
	for _, r := range rows {
		totalWithDeleted += r.Count
	}
	assert.Equal(int64(5), totalWithDeleted, "expected 5 messages (deleted included)")

	// With HideDeletedFromSource: deleted messages excluded
	opts.HideDeletedFromSource = true
	rows, err = env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	require.NoError(err, "Aggregate (hide deleted)")
	var totalHidden int64
	for _, r := range rows {
		totalHidden += r.Count
	}
	assert.Equal(int64(4), totalHidden, "expected 4 messages (deleted hidden)")

	// SubAggregate with HideDeletedFromSource
	filter := MessageFilter{Sender: "alice@example.com", HideDeletedFromSource: true}
	subRows, err := env.Engine.SubAggregate(env.Ctx, filter, ViewRecipients, DefaultAggregateOptions())
	require.NoError(err, "SubAggregate (hide deleted)")
	var subTotal int64
	for _, r := range subRows {
		subTotal += r.Count
	}
	// alice has 3 messages, but message 1 is deleted, so 2 should remain
	assert.Equal(int64(2), subTotal, "expected 2 messages for alice (deleted hidden)")
}

func TestSubAggregateByTime(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	filter := MessageFilter{Sender: "alice@example.com"}
	opts := DefaultAggregateOptions()
	opts.TimeGranularity = TimeMonth

	results, err := env.Engine.SubAggregate(env.Ctx, filter, ViewTime, opts)
	requirepkg.NoError(t, err, "SubAggregate")

	assert.Len(results, 2, "expected 2 time periods for alice@example.com's messages")

	for _, r := range results {
		assert.Len(r.Key, 7, "expected YYYY-MM format")
		if len(r.Key) >= 5 {
			assert.Equal(byte('-'), r.Key[4], "expected YYYY-MM format")
		}
	}
}

// =============================================================================
// RecipientName aggregate tests
// =============================================================================

func TestAggregateByRecipientName_FallbackToEmail(t *testing.T) {
	env := newTestEnv(t)

	// Resolve participant IDs dynamically to avoid coupling to seed order.
	aliceID := env.MustLookupParticipant("alice@example.com")

	noNameID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("noname@test.com"), DisplayName: nil, Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "No Name Recipient", SentAt: "2024-05-01 10:00:00", FromID: aliceID, ToIDs: []int64{noNameID}})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "AggregateByRecipientName")

	assertRow(t, rows, "noname@test.com")
}

func TestAggregateByRecipientName_EmptyStringFallback(t *testing.T) {
	env := newTestEnv(t)

	// Resolve participant IDs dynamically to avoid coupling to seed order.
	aliceID := env.MustLookupParticipant("alice@example.com")

	emptyID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("empty@test.com"), DisplayName: new(""), Domain: "test.com"})
	spacesID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("spaces@test.com"), DisplayName: new("   "), Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "Empty Rcpt Name", SentAt: "2024-05-01 10:00:00", FromID: aliceID, ToIDs: []int64{emptyID}})
	env.AddMessage(dbtest.MessageOpts{Subject: "Spaces Rcpt Name", SentAt: "2024-05-02 10:00:00", FromID: aliceID, CcIDs: []int64{spacesID}})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "AggregateByRecipientName")

	assertRowsContain(t, rows, []aggExpectation{
		{"empty@test.com", 1},
		{"spaces@test.com", 1},
	})
}

// =============================================================================
// Invalid ViewType tests
// =============================================================================

// TestSQLiteEngine_Aggregate_InvalidViewType verifies that invalid ViewType values
// return a clear error from the Aggregate API.
func TestSQLiteEngine_Aggregate_InvalidViewType(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name     string
		viewType ViewType
	}{
		{name: "ViewTypeCount", viewType: ViewTypeCount},
		{name: "NegativeValue", viewType: ViewType(-1)},
		{name: "LargeValue", viewType: ViewType(999)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := env.Engine.Aggregate(env.Ctx, tt.viewType, DefaultAggregateOptions())
			requirepkg.ErrorContains(t, err, "unsupported view type")
		})
	}
}

// TestAggregateDeterministicOrderOnTies verifies that when aggregate values tie
// (e.g., two labels with equal counts), results are sorted deterministically by key ASC.
// This prevents flaky tests and non-deterministic UI ordering.
func TestAggregateDeterministicOrderOnTies(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")

	// Create minimal test data using helpers, explicitly threading IDs to avoid
	// implicit coupling to helper defaults or auto-increment assumptions.
	sourceID := tdb.AddSource(dbtest.SourceOpts{Identifier: "test@gmail.com", DisplayName: "Test Account"})
	convID := tdb.AddConversation(dbtest.ConversationOpts{SourceID: sourceID, Title: "Test Thread"})
	aliceID := tdb.AddParticipant(dbtest.ParticipantOpts{Email: new("alice@example.com"), DisplayName: new("Alice"), Domain: "example.com"})
	bobID := tdb.AddParticipant(dbtest.ParticipantOpts{Email: new("bob@example.com"), DisplayName: new("Bob"), Domain: "example.com"})

	// Create labels with names that would sort differently than insertion order
	// "Zebra" inserted first, "Apple" inserted second - both will have count=1
	zebraID := tdb.AddLabel(dbtest.LabelOpts{Name: "Zebra"})
	appleID := tdb.AddLabel(dbtest.LabelOpts{Name: "Apple"})

	// Add one message with both labels
	msgID := tdb.AddMessage(dbtest.MessageOpts{
		Subject:        "Test",
		SentAt:         "2024-01-01 10:00:00",
		FromID:         aliceID,
		ToIDs:          []int64{bobID},
		SourceID:       sourceID,
		ConversationID: convID,
	})
	tdb.AddMessageLabel(msgID, zebraID)
	tdb.AddMessageLabel(msgID, appleID)

	env := &testEnv{
		TestDB: tdb,
		Engine: NewSQLiteEngine(tdb.DB),
		Ctx:    context.Background(),
	}

	// Default sort is by count DESC. Both labels have count=1, so they should
	// be ordered by key ASC as secondary sort: Apple before Zebra.
	opts := DefaultAggregateOptions()
	rows, err := env.Engine.Aggregate(env.Ctx, ViewLabels, opts)
	requirepkg.NoError(t, err, "Aggregate")

	// Verify exact order: Apple (count=1) then Zebra (count=1)
	assertAggRows(t, rows, []aggExpectation{
		{"Apple", 1},
		{"Zebra", 1},
	})
}

// TestAggregateByLabel_WithSearchQuery verifies that label: search in the
// Labels aggregate view only shows matching labels (case-insensitive substring).
func TestAggregateByLabel_WithSearchQuery(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name       string
		query      string
		wantLabels []string
	}{
		{
			name:       "case_insensitive",
			query:      "label:work",
			wantLabels: []string{"Work"},
		},
		{
			name:       "substring",
			query:      "label:wor",
			wantLabels: []string{"Work"},
		},
		{
			name:       "multi_label_or",
			query:      "label:work label:important",
			wantLabels: []string{"Work", "IMPORTANT"},
		},
		{
			name:       "no_match",
			query:      "label:nonexistent",
			wantLabels: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := DefaultAggregateOptions()
			opts.SearchQuery = tt.query
			rows, err := env.Engine.Aggregate(
				env.Ctx, ViewLabels, opts,
			)
			requirepkg.NoError(t, err, "Aggregate")
			gotLabels := make([]string, 0, len(rows))
			for _, r := range rows {
				gotLabels = append(gotLabels, r.Key)
			}
			assertpkg.ElementsMatch(t, tt.wantLabels, gotLabels)
		})
	}
}

// TestSubAggregate_WithSearchQuery verifies that SubAggregate applies
// SearchQuery to filter results (not silently dropped).
func TestSubAggregate_WithSearchQuery(t *testing.T) {
	env := newTestEnv(t)

	// SubAggregate by Labels under sender alice, search "work"
	opts := DefaultAggregateOptions()
	opts.SearchQuery = "label:work"
	filter := MessageFilter{Sender: "alice@example.com"}
	rows, err := env.Engine.SubAggregate(
		env.Ctx, filter, ViewLabels, opts,
	)
	requirepkg.NoError(t, err, "SubAggregate")
	// Should return exactly the "Work" label, not all labels for alice
	requirepkg.Len(t, rows, 1, "expected 1 label row")
	assertpkg.Equal(t, "Work", rows[0].Key)
}

// TestEscapeSQLiteLike verifies that wildcard characters are escaped
// so they match literally in label search.
func TestEscapeSQLiteLike(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"work", "work"},
		{"100%", `100\%`},
		{"in_box", `in\_box`},
		{`path\raw`, `path\\raw`},
		{`%_\`, `\%\_\\`},
	}
	for _, tt := range tests {
		got := escapeSQLiteLike(tt.input)
		assertpkg.Equal(t, tt.want, got, "escapeSQLiteLike(%q)", tt.input)
	}
}

// TestAggregateBySender_RecipientFilterNoOvercount verifies that recipient
// EXISTS filters don't inflate counts when a message has multiple matching
// recipients. Message 1 has to:bob AND to:carol; a search matching both
// must still count message 1 once.
func TestAggregateBySender_RecipientFilterNoOvercount(t *testing.T) {
	env := newTestEnv(t)

	opts := DefaultAggregateOptions()
	// Message 1 (from alice) has to:bob AND to:carol. Searching for
	// both terms exercises the multi-recipient path: with old JOIN-based
	// filters, message 1 would match both joins and be double-counted.
	opts.SearchQuery = "to:bob@company.org to:carol@example.com"
	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	requirepkg.NoError(t, err, "Aggregate")

	m := aggRowMap(t, rows)
	// to: terms use OR — messages 1,2,3 match (all have to:bob, msg 1
	// also has to:carol). With old JOIN filters, message 1 would produce
	// two joined rows (matching bob and carol), inflating to count 4.
	assertpkg.Equal(t, int64(3), m["alice@example.com"], "alice count (no overcount)")
}

// TestSQLiteEngine_SubAggregate_InvalidViewType verifies that invalid ViewType values
// return a clear error from the SubAggregate API.
func TestSQLiteEngine_SubAggregate_InvalidViewType(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name     string
		viewType ViewType
	}{
		{name: "ViewTypeCount", viewType: ViewTypeCount},
		{name: "NegativeValue", viewType: ViewType(-1)},
		{name: "LargeValue", viewType: ViewType(999)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := MessageFilter{Sender: "alice@example.com"}
			_, err := env.Engine.SubAggregate(env.Ctx, filter, tt.viewType, DefaultAggregateOptions())
			requirepkg.ErrorContains(t, err, "unsupported view type")
		})
	}
}
