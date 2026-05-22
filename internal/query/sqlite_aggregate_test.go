package query

import (
	"context"
	"strings"
	"testing"
	"time"

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
			if err != nil {
				t.Fatalf("%s: %v", tc.aggName, err)
			}
			assertAggRows(t, rows, tc.want)
		})
	}
}

func TestAggregateBySenderName_FallbackToEmail(t *testing.T) {
	env := newTestEnv(t)

	noNameID := env.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("noname@test.com"), DisplayName: nil, Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "No Name Test", SentAt: "2024-05-01 10:00:00", FromID: noNameID})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenderNames, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("AggregateBySenderName: %v", err)
	}

	if len(rows) != 3 {
		t.Errorf("expected 3 sender names, got %d", len(rows))
	}

	assertRow(t, rows, "noname@test.com", 1)
}

// TestAggregateBySenderName_FallbackToPhone covers phone-only iMessage/SMS
// participants without a contacts entry: display_name and email_address are
// both empty/NULL but phone_number is set. They must surface under the phone
// number, not vanish from the SenderNames aggregate.
func TestAggregateBySenderName_FallbackToPhone(t *testing.T) {
	env := newTestEnv(t)

	phoneOnlyID := env.AddParticipant(dbtest.ParticipantOpts{
		Phone:       dbtest.StrPtr("+15551234567"),
		DisplayName: dbtest.StrPtr(""),
	})
	env.AddMessage(dbtest.MessageOpts{Subject: "SMS", SentAt: "2024-05-01 10:00:00", FromID: phoneOnlyID})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenderNames, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("AggregateBySenderName: %v", err)
	}

	assertRow(t, rows, "+15551234567", 1)

	// Same fallback drives the SenderName filter.
	listed := env.MustListMessages(MessageFilter{SenderName: "+15551234567"})
	if len(listed) != 1 {
		t.Errorf("ListMessages by phone-fallback name: got %d, want 1", len(listed))
	}
}

// TestAggregateByRecipientName_FallbackToPhone is the recipient-side analog.
func TestAggregateByRecipientName_FallbackToPhone(t *testing.T) {
	env := newTestEnv(t)

	phoneOnlyID := env.AddParticipant(dbtest.ParticipantOpts{
		Phone:       dbtest.StrPtr("+15557654321"),
		DisplayName: dbtest.StrPtr(""),
	})
	senderID := env.MustLookupParticipant("alice@example.com")
	env.AddMessage(dbtest.MessageOpts{
		Subject: "Group", SentAt: "2024-05-01 10:00:00",
		FromID: senderID, ToIDs: []int64{phoneOnlyID},
	})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("AggregateByRecipientName: %v", err)
	}

	assertRow(t, rows, "+15557654321", 1)

	listed := env.MustListMessages(MessageFilter{RecipientName: "+15557654321"})
	if len(listed) != 1 {
		t.Errorf("ListMessages by phone-fallback recipient name: got %d, want 1", len(listed))
	}
}

func TestAggregateBySenderName_EmptyStringFallback(t *testing.T) {
	env := newTestEnv(t)

	emptyID := env.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("empty@test.com"), DisplayName: dbtest.StrPtr(""), Domain: "test.com"})
	spacesID := env.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("spaces@test.com"), DisplayName: dbtest.StrPtr("   "), Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "Empty Name", SentAt: "2024-05-01 10:00:00", FromID: emptyID})
	env.AddMessage(dbtest.MessageOpts{Subject: "Spaces Name", SentAt: "2024-05-02 10:00:00", FromID: spacesID})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenderNames, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("AggregateBySenderName: %v", err)
	}

	if len(rows) != 4 {
		t.Errorf("expected 4 sender names, got %d", len(rows))
		for _, r := range rows {
			t.Logf("  key=%q count=%d", r.Key, r.Count)
		}
	}

	for _, r := range rows {
		if r.Key == "" || r.Key == "   " {
			t.Errorf("unexpected empty/whitespace key: %q", r.Key)
		}
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
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}

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
	if err != nil {
		t.Fatalf("AggregateBySender with date filter: %v", err)
	}

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
		if err != nil {
			t.Fatalf("AggregateBySender: %v", err)
		}
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
		if err != nil {
			t.Fatalf("AggregateBySender: %v", err)
		}
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
	if err != nil {
		t.Fatalf("AggregateBySender: %v", err)
	}

	assertRowsContain(t, allRows, []aggExpectation{
		{"alice@example.com", 3},
		{"bob@company.org", 2},
	})

	opts.WithAttachmentsOnly = true
	attRows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	if err != nil {
		t.Fatalf("AggregateBySender with attachment filter: %v", err)
	}

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
			if err != nil {
				t.Fatalf("SubAggregate: %v", err)
			}
			assertAggRows(t, results, tc.want)
		})
	}
}

func TestSubAggregate_MatchEmptySenderName(t *testing.T) {
	env := newTestEnvWithEmptyBuckets(t)

	filter := MessageFilter{EmptyValueTargets: map[ViewType]bool{ViewSenderNames: true}}
	results, err := env.Engine.SubAggregate(env.Ctx, filter, ViewLabels, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("SubAggregate with MatchEmptySenderName: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 label sub-aggregates for empty sender name, got %d", len(results))
		for _, r := range results {
			t.Logf("  key=%q count=%d", r.Key, r.Count)
		}
	}
}

func TestSubAggregateIncludesDeletedMessages(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{Sender: "alice@example.com"}
	resultsBefore, err := env.Engine.SubAggregate(env.Ctx, filter, ViewRecipients, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("SubAggregate before: %v", err)
	}

	env.MarkDeletedByID(1)

	resultsAfter, err := env.Engine.SubAggregate(env.Ctx, filter, ViewRecipients, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("SubAggregate after: %v", err)
	}

	var totalBefore, totalAfter int64
	for _, r := range resultsBefore {
		totalBefore += r.Count
	}
	for _, r := range resultsAfter {
		totalAfter += r.Count
	}

	if totalAfter != totalBefore {
		t.Errorf("expected same message count (deleted included), before=%d after=%d", totalBefore, totalAfter)
	}
}

func TestHideDeletedFromSourceAggregate(t *testing.T) {
	env := newTestEnv(t)

	// Before deletion: all 5 messages visible
	opts := DefaultAggregateOptions()
	allRows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var totalBefore int64
	for _, r := range allRows {
		totalBefore += r.Count
	}
	if totalBefore != 5 {
		t.Fatalf("expected 5 total messages before deletion, got %d", totalBefore)
	}

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Without HideDeletedFromSource: deleted messages still included
	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	if err != nil {
		t.Fatalf("Aggregate (no hide): %v", err)
	}
	var totalWithDeleted int64
	for _, r := range rows {
		totalWithDeleted += r.Count
	}
	if totalWithDeleted != 5 {
		t.Errorf("expected 5 messages (deleted included), got %d", totalWithDeleted)
	}

	// With HideDeletedFromSource: deleted messages excluded
	opts.HideDeletedFromSource = true
	rows, err = env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	if err != nil {
		t.Fatalf("Aggregate (hide deleted): %v", err)
	}
	var totalHidden int64
	for _, r := range rows {
		totalHidden += r.Count
	}
	if totalHidden != 4 {
		t.Errorf("expected 4 messages (deleted hidden), got %d", totalHidden)
	}

	// SubAggregate with HideDeletedFromSource
	filter := MessageFilter{Sender: "alice@example.com", HideDeletedFromSource: true}
	subRows, err := env.Engine.SubAggregate(env.Ctx, filter, ViewRecipients, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("SubAggregate (hide deleted): %v", err)
	}
	var subTotal int64
	for _, r := range subRows {
		subTotal += r.Count
	}
	// alice has 3 messages, but message 1 is deleted, so 2 should remain
	if subTotal != 2 {
		t.Errorf("expected 2 messages for alice (deleted hidden), got %d", subTotal)
	}
}

func TestSubAggregateByTime(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{Sender: "alice@example.com"}
	opts := DefaultAggregateOptions()
	opts.TimeGranularity = TimeMonth

	results, err := env.Engine.SubAggregate(env.Ctx, filter, ViewTime, opts)
	if err != nil {
		t.Fatalf("SubAggregate: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 time periods for alice@example.com's messages, got %d", len(results))
	}

	for _, r := range results {
		if len(r.Key) != 7 || r.Key[4] != '-' {
			t.Errorf("expected YYYY-MM format, got %q", r.Key)
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

	noNameID := env.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("noname@test.com"), DisplayName: nil, Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "No Name Recipient", SentAt: "2024-05-01 10:00:00", FromID: aliceID, ToIDs: []int64{noNameID}})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("AggregateByRecipientName: %v", err)
	}

	assertRow(t, rows, "noname@test.com", 1)
}

func TestAggregateByRecipientName_EmptyStringFallback(t *testing.T) {
	env := newTestEnv(t)

	// Resolve participant IDs dynamically to avoid coupling to seed order.
	aliceID := env.MustLookupParticipant("alice@example.com")

	emptyID := env.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("empty@test.com"), DisplayName: dbtest.StrPtr(""), Domain: "test.com"})
	spacesID := env.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("spaces@test.com"), DisplayName: dbtest.StrPtr("   "), Domain: "test.com"})
	env.AddMessage(dbtest.MessageOpts{Subject: "Empty Rcpt Name", SentAt: "2024-05-01 10:00:00", FromID: aliceID, ToIDs: []int64{emptyID}})
	env.AddMessage(dbtest.MessageOpts{Subject: "Spaces Rcpt Name", SentAt: "2024-05-02 10:00:00", FromID: aliceID, CcIDs: []int64{spacesID}})

	rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("AggregateByRecipientName: %v", err)
	}

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
			if err == nil {
				t.Fatal("expected error for invalid ViewType, got nil")
			}
			if !strings.Contains(err.Error(), "unsupported view type") {
				t.Errorf("expected 'unsupported view type' error, got: %v", err)
			}
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
	aliceID := tdb.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("alice@example.com"), DisplayName: dbtest.StrPtr("Alice"), Domain: "example.com"})
	bobID := tdb.AddParticipant(dbtest.ParticipantOpts{Email: dbtest.StrPtr("bob@example.com"), DisplayName: dbtest.StrPtr("Bob"), Domain: "example.com"})

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
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

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
			if err != nil {
				t.Fatalf("Aggregate: %v", err)
			}
			gotLabels := make([]string, 0, len(rows))
			for _, r := range rows {
				gotLabels = append(gotLabels, r.Key)
			}
			if len(gotLabels) != len(tt.wantLabels) {
				t.Errorf("got %d labels %v, want %d %v",
					len(gotLabels), gotLabels,
					len(tt.wantLabels), tt.wantLabels)
				return
			}
			for i, want := range tt.wantLabels {
				if gotLabels[i] != want {
					t.Errorf("label[%d] = %q, want %q",
						i, gotLabels[i], want)
				}
			}
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
	if err != nil {
		t.Fatalf("SubAggregate: %v", err)
	}
	// Should return exactly the "Work" label, not all labels for alice
	if len(rows) != 1 {
		t.Fatalf("expected 1 label row, got %d: %v", len(rows), rows)
	}
	if rows[0].Key != "Work" {
		t.Errorf("expected label Work, got %q", rows[0].Key)
	}
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
		if got != tt.want {
			t.Errorf("escapeSQLiteLike(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
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
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	m := aggRowMap(t, rows)
	// to: terms use OR — messages 1,2,3 match (all have to:bob, msg 1
	// also has to:carol). With old JOIN filters, message 1 would produce
	// two joined rows (matching bob and carol), inflating to count 4.
	if got := m["alice@example.com"]; got != 3 {
		t.Errorf("alice count = %d, want 3 (no overcount)", got)
	}
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
			if err == nil {
				t.Fatal("expected error for invalid ViewType, got nil")
			}
			if !strings.Contains(err.Error(), "unsupported view type") {
				t.Errorf("expected 'unsupported view type' error, got: %v", err)
			}
		})
	}
}
