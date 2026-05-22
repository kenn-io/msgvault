package query

import (
	"bytes"
	"testing"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

// emptyTargets creates an EmptyValueTargets map for testing with the given ViewType(s).
func emptyTargets(views ...ViewType) map[ViewType]bool {
	m := make(map[ViewType]bool)
	for _, v := range views {
		m[v] = true
	}
	return m
}

// TestMessageFilter_Clone verifies that Clone creates an independent copy
// of the filter, especially the EmptyValueTargets map.
func TestMessageFilter_Clone(t *testing.T) {
	// Create original filter with EmptyValueTargets
	original := MessageFilter{
		Sender: "alice@example.com",
		Label:  "INBOX",
		EmptyValueTargets: map[ViewType]bool{
			ViewSenders: true,
		},
	}

	// Clone it
	clone := original.Clone()

	// Verify scalar fields are copied
	if clone.Sender != "alice@example.com" {
		t.Errorf("expected Sender 'alice@example.com', got %q", clone.Sender)
	}
	if clone.Label != "INBOX" {
		t.Errorf("expected Label 'INBOX', got %q", clone.Label)
	}

	// Verify EmptyValueTargets is deeply copied
	if !clone.MatchesEmpty(ViewSenders) {
		t.Error("clone should have ViewSenders in EmptyValueTargets")
	}

	// Mutate the clone's map
	clone.SetEmptyTarget(ViewLabels)

	// Verify original is NOT affected
	if original.MatchesEmpty(ViewLabels) {
		t.Error("original should NOT have ViewLabels after mutating clone")
	}

	// Mutate the original's map
	original.SetEmptyTarget(ViewDomains)

	// Verify clone is NOT affected
	if clone.MatchesEmpty(ViewDomains) {
		t.Error("clone should NOT have ViewDomains after mutating original")
	}
}

// TestMessageFilter_Clone_NilMap verifies Clone handles nil EmptyValueTargets.
func TestMessageFilter_Clone_NilMap(t *testing.T) {
	original := MessageFilter{Sender: "bob@example.com"}
	clone := original.Clone()

	if clone.Sender != "bob@example.com" {
		t.Errorf("expected Sender 'bob@example.com', got %q", clone.Sender)
	}
	if clone.EmptyValueTargets != nil {
		t.Errorf("expected nil EmptyValueTargets, got %v", clone.EmptyValueTargets)
	}

	// Mutating clone should not affect original
	clone.SetEmptyTarget(ViewSenders)
	if original.EmptyValueTargets != nil {
		t.Errorf("original EmptyValueTargets should still be nil")
	}
}

// TestMessageFilter_HasEmptyTargets verifies HasEmptyTargets checks for true values.
func TestMessageFilter_HasEmptyTargets(t *testing.T) {
	tests := []struct {
		name   string
		filter MessageFilter
		want   bool
	}{
		{
			name:   "nil map",
			filter: MessageFilter{},
			want:   false,
		},
		{
			name:   "empty map",
			filter: MessageFilter{EmptyValueTargets: map[ViewType]bool{}},
			want:   false,
		},
		{
			name:   "map with only false values",
			filter: MessageFilter{EmptyValueTargets: map[ViewType]bool{ViewSenders: false, ViewLabels: false}},
			want:   false,
		},
		{
			name:   "map with one true value",
			filter: MessageFilter{EmptyValueTargets: map[ViewType]bool{ViewSenders: true}},
			want:   true,
		},
		{
			name:   "map with mixed true and false",
			filter: MessageFilter{EmptyValueTargets: map[ViewType]bool{ViewSenders: false, ViewLabels: true}},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.filter.HasEmptyTargets()
			if got != tt.want {
				t.Errorf("HasEmptyTargets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListMessages_Filters(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name      string
		filter    MessageFilter
		wantCount int
		validate  func(*testing.T, []MessageSummary)
	}{
		{
			name:      "All messages",
			filter:    MessageFilter{},
			wantCount: 5,
		},
		{
			name:      "Filter by sender",
			filter:    MessageFilter{Sender: "alice@example.com"},
			wantCount: 3,
		},
		{
			name:      "Filter by label",
			filter:    MessageFilter{Label: "Work"},
			wantCount: 2,
		},
		{
			name:      "Filter by label case-insensitive",
			filter:    MessageFilter{Label: "work"},
			wantCount: 2,
		},
		{
			name:      "Filter by sender name",
			filter:    MessageFilter{SenderName: "Alice Smith"},
			wantCount: 3,
		},
		{
			name:      "Filter by recipient name",
			filter:    MessageFilter{RecipientName: "Bob Jones"},
			wantCount: 3,
		},
		{
			name:      "Combined recipient and recipient name",
			filter:    MessageFilter{Recipient: "bob@company.org", RecipientName: "Bob Jones"},
			wantCount: 3,
		},
		{
			name:      "Mismatched recipient and recipient name",
			filter:    MessageFilter{Recipient: "bob@company.org", RecipientName: "Alice Smith"},
			wantCount: 0,
		},
		{
			name:      "RecipientName with MatchEmptyRecipient (contradictory)",
			filter:    MessageFilter{RecipientName: "Bob Jones", EmptyValueTargets: emptyTargets(ViewRecipients)},
			wantCount: 0,
		},
		{
			name:      "MatchEmptyRecipientName with sender",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewRecipientNames), Sender: "alice@example.com"},
			wantCount: 0,
		},
		{
			name:      "Time period month",
			filter:    MessageFilter{TimeRange: TimeRange{Period: "2024-01"}},
			wantCount: 2,
		},
		{
			name:      "Time period day",
			filter:    MessageFilter{TimeRange: TimeRange{Period: "2024-01-15"}},
			wantCount: 1,
		},
		{
			name:      "Time period year",
			filter:    MessageFilter{TimeRange: TimeRange{Period: "2024", Granularity: TimeYear}},
			wantCount: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := env.MustListMessages(tt.filter)
			if len(messages) != tt.wantCount {
				t.Errorf("got %d messages, want %d", len(messages), tt.wantCount)
			}
			if tt.validate != nil {
				tt.validate(t, messages)
			}
		})
	}
}

func TestListMessages_NoDuplicates(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{Recipient: "bob@company.org", RecipientName: "Bob Jones"}
	messages := env.MustListMessages(filter)

	seen := make(map[int64]int)
	for _, m := range messages {
		seen[m.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("message ID %d returned %d times (expected once)", id, count)
		}
	}
}

func TestListMessagesWithLabels(t *testing.T) {
	env := newTestEnv(t)

	messages := env.MustListMessages(MessageFilter{})

	msg1 := messages[len(messages)-1]
	if len(msg1.Labels) != 2 {
		t.Errorf("expected 2 labels on msg1, got %d", len(msg1.Labels))
	}
}

func TestGetMessage(t *testing.T) {
	env := newTestEnv(t)

	msg, err := env.Engine.GetMessage(env.Ctx, 1)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg == nil {
		t.Fatal("expected message, got nil")
	}
	if msg.Subject != "Hello World" {
		t.Errorf("expected subject 'Hello World', got %q", msg.Subject)
	}
	if len(msg.From) != 1 || msg.From[0].Email != "alice@example.com" {
		t.Errorf("expected from alice, got %v", msg.From)
	}
	if len(msg.To) != 2 {
		t.Errorf("expected 2 recipients, got %d", len(msg.To))
	}
	if len(msg.Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(msg.Labels))
	}
	if msg.BodyText != "Message body 1" {
		t.Errorf("expected body text 'Message body 1', got %q", msg.BodyText)
	}
}

func TestGetMessageWithAttachments(t *testing.T) {
	env := newTestEnv(t)

	msg, err := env.Engine.GetMessage(env.Ctx, 2)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if len(msg.Attachments) != 2 {
		t.Errorf("expected 2 attachments, got %d", len(msg.Attachments))
	}
	found := false
	for _, att := range msg.Attachments {
		if att.Filename == "doc.pdf" {
			found = true
			if att.MimeType != "application/pdf" {
				t.Errorf("expected mime type application/pdf, got %s", att.MimeType)
			}
			if att.Size != 10000 {
				t.Errorf("expected size 10000, got %d", att.Size)
			}
		}
	}
	if !found {
		t.Error("expected to find doc.pdf attachment")
	}
}

func TestGetMessageBySourceID(t *testing.T) {
	env := newTestEnv(t)

	msg, err := env.Engine.GetMessageBySourceID(env.Ctx, "msg3")
	if err != nil {
		t.Fatalf("GetMessageBySourceID: %v", err)
	}
	if msg == nil {
		t.Fatal("expected message, got nil")
	}
	if msg.Subject != "Follow up" {
		t.Errorf("expected subject 'Follow up', got %q", msg.Subject)
	}
}

func TestListAccounts(t *testing.T) {
	env := newTestEnv(t)

	accounts, err := env.Engine.ListAccounts(env.Ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].Identifier != "test@gmail.com" {
		t.Errorf("expected test@gmail.com, got %s", accounts[0].Identifier)
	}
}

func TestGetTotalStats(t *testing.T) {
	env := newTestEnv(t)

	stats := env.MustGetTotalStats(StatsOptions{})

	if stats.MessageCount != 5 {
		t.Errorf("expected 5 messages, got %d", stats.MessageCount)
	}
	if stats.AttachmentCount != 3 {
		t.Errorf("expected 3 attachments, got %d", stats.AttachmentCount)
	}
	expectedSize := int64(1000 + 2000 + 1500 + 3000 + 500)
	if stats.TotalSize != expectedSize {
		t.Errorf("expected total size %d, got %d", expectedSize, stats.TotalSize)
	}
	expectedAttSize := int64(10000 + 5000 + 20000)
	if stats.AttachmentSize != expectedAttSize {
		t.Errorf("expected attachment size %d, got %d", expectedAttSize, stats.AttachmentSize)
	}
}

func TestGetTotalStatsWithSourceID(t *testing.T) {
	env := newTestEnv(t)

	src2 := env.AddSource(dbtest.SourceOpts{Identifier: "other@gmail.com", DisplayName: "Other Account"})
	env.AddLabel(dbtest.LabelOpts{SourceID: src2, SourceLabelID: "INBOX", Name: "INBOX", Type: "system"})
	env.AddLabel(dbtest.LabelOpts{SourceID: src2, SourceLabelID: "personal", Name: "Personal"})
	conv2 := env.AddConversation(dbtest.ConversationOpts{SourceID: src2, Title: "Other Thread"})
	env.AddMessage(dbtest.MessageOpts{
		SourceID:       src2,
		ConversationID: conv2,
		Subject:        "Other msg",
		SentAt:         "2024-01-20 10:00:00",
		SizeEstimate:   500,
	})

	allStats := env.MustGetTotalStats(StatsOptions{})
	if allStats.MessageCount != 6 {
		t.Errorf("expected 6 total messages, got %d", allStats.MessageCount)
	}
	if allStats.LabelCount != 5 {
		t.Errorf("expected 5 total labels, got %d", allStats.LabelCount)
	}
	if allStats.AccountCount != 2 {
		t.Errorf("expected 2 accounts, got %d", allStats.AccountCount)
	}

	sourceID := int64(1)
	source1Stats := env.MustGetTotalStats(StatsOptions{SourceID: &sourceID})
	if source1Stats.MessageCount != 5 {
		t.Errorf("expected 5 messages for source 1, got %d", source1Stats.MessageCount)
	}
	if source1Stats.LabelCount != 3 {
		t.Errorf("expected 3 labels for source 1, got %d", source1Stats.LabelCount)
	}
	if source1Stats.AccountCount != 1 {
		t.Errorf("expected account count 1 when filtering by source, got %d", source1Stats.AccountCount)
	}
}

func TestGetTotalStatsWithInvalidSourceID(t *testing.T) {
	env := newTestEnv(t)

	nonExistentID := int64(9999)
	stats := env.MustGetTotalStats(StatsOptions{SourceID: &nonExistentID})

	if stats.MessageCount != 0 {
		t.Errorf("expected 0 messages for non-existent source, got %d", stats.MessageCount)
	}
	if stats.LabelCount != 0 {
		t.Errorf("expected 0 labels for non-existent source, got %d", stats.LabelCount)
	}
	if stats.AccountCount != 0 {
		t.Errorf("expected 0 account count for non-existent source, got %d", stats.AccountCount)
	}
	if stats.AttachmentCount != 0 {
		t.Errorf("expected 0 attachments for non-existent source, got %d", stats.AttachmentCount)
	}
}

func TestWithAttachmentsOnlyStats(t *testing.T) {
	env := newTestEnv(t)

	allStats := env.MustGetTotalStats(StatsOptions{})
	if allStats.MessageCount != 5 {
		t.Errorf("expected 5 total messages, got %d", allStats.MessageCount)
	}

	attStats := env.MustGetTotalStats(StatsOptions{WithAttachmentsOnly: true})
	if attStats.MessageCount != 2 {
		t.Errorf("expected 2 messages with attachments, got %d", attStats.MessageCount)
	}
	if attStats.AttachmentCount == 0 {
		t.Error("expected non-zero attachment count for messages with attachments")
	}
}

func TestHideDeletedFromSourceSearchFast(t *testing.T) {
	env := newTestEnv(t)

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Use an empty query (matches all messages)
	q := &search.Query{}

	// SearchFast without filter: all 5 messages
	all, err := env.Engine.SearchFast(env.Ctx, q, MessageFilter{}, 100, 0)
	if err != nil {
		t.Fatalf("SearchFast: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("SearchFast without filter: expected 5, got %d", len(all))
	}

	// SearchFast with HideDeletedFromSource: 4 messages
	hidden, err := env.Engine.SearchFast(env.Ctx, q, MessageFilter{HideDeletedFromSource: true}, 100, 0)
	if err != nil {
		t.Fatalf("SearchFast(hide-deleted): %v", err)
	}
	if len(hidden) != 4 {
		t.Errorf("SearchFast with hide-deleted: expected 4, got %d", len(hidden))
	}

	// SearchFastCount must agree
	count, err := env.Engine.SearchFastCount(env.Ctx, q, MessageFilter{HideDeletedFromSource: true})
	if err != nil {
		t.Fatalf("SearchFastCount(hide-deleted): %v", err)
	}
	if count != 4 {
		t.Errorf("SearchFastCount with hide-deleted: expected 4, got %d", count)
	}
}

func TestHideDeletedFromSourceStats(t *testing.T) {
	env := newTestEnv(t)

	allStats := env.MustGetTotalStats(StatsOptions{})
	if allStats.MessageCount != 5 {
		t.Errorf("expected 5 total messages, got %d", allStats.MessageCount)
	}

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Without HideDeletedFromSource: still 5
	stats := env.MustGetTotalStats(StatsOptions{})
	if stats.MessageCount != 5 {
		t.Errorf("expected 5 messages (deleted included), got %d", stats.MessageCount)
	}

	// With HideDeletedFromSource: 4
	hiddenStats := env.MustGetTotalStats(StatsOptions{HideDeletedFromSource: true})
	if hiddenStats.MessageCount != 4 {
		t.Errorf("expected 4 messages (deleted hidden), got %d", hiddenStats.MessageCount)
	}
}

func TestDeletedMessagesIncludedWithFlag(t *testing.T) {
	env := newTestEnv(t)

	env.MarkDeletedByID(1)

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("Aggregate(ViewSenders): %v", err)
	}
	for _, row := range rows {
		if row.Key == "alice@example.com" && row.Count != 3 {
			t.Errorf("expected alice count 3 (including deleted), got %d", row.Count)
		}
	}

	messages := env.MustListMessages(MessageFilter{})
	if len(messages) != 5 {
		t.Errorf("expected 5 messages (including deleted), got %d", len(messages))
	}

	var foundDeleted bool
	for _, msg := range messages {
		if msg.ID == 1 {
			if msg.DeletedAt == nil {
				t.Error("expected DeletedAt to be set for deleted message")
			}
			foundDeleted = true
		} else {
			if msg.DeletedAt != nil {
				t.Errorf("expected DeletedAt to be nil for non-deleted message %d", msg.ID)
			}
		}
	}
	if !foundDeleted {
		t.Error("deleted message not found in results")
	}

	stats := env.MustGetTotalStats(StatsOptions{})
	if stats.MessageCount != 5 {
		t.Errorf("expected 5 messages in stats (including deleted), got %d", stats.MessageCount)
	}
}

func TestGetMessageIncludesDeleted(t *testing.T) {
	env := newTestEnv(t)

	env.MarkDeletedByID(1)

	msg, err := env.Engine.GetMessage(env.Ctx, 1)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg == nil {
		t.Error("expected deleted message to be returned, got nil")
	}

	msg, err = env.Engine.GetMessage(env.Ctx, 2)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg == nil {
		t.Error("expected message, got nil")
	}
}

func TestGetMessageBySourceIDIncludesDeleted(t *testing.T) {
	env := newTestEnv(t)

	env.MarkDeletedBySourceID("msg3")

	msg, err := env.Engine.GetMessageBySourceID(env.Ctx, "msg3")
	if err != nil {
		t.Fatalf("GetMessageBySourceID: %v", err)
	}
	if msg == nil {
		t.Error("expected deleted message to be returned, got nil")
	}

	msg, err = env.Engine.GetMessageBySourceID(env.Ctx, "msg2")
	if err != nil {
		t.Fatalf("GetMessageBySourceID: %v", err)
	}
	if msg == nil {
		t.Error("expected message, got nil")
	}
}

func TestListMessages_MatchEmptySenderName_NotExists(t *testing.T) {
	env := newTestEnv(t)

	env.AddMessage(dbtest.MessageOpts{Subject: "Ghost Message", SentAt: "2024-06-01 10:00:00"})

	filter := MessageFilter{EmptyValueTargets: emptyTargets(ViewSenderNames)}
	messages := env.MustListMessages(filter)

	if len(messages) != 1 {
		t.Errorf("expected 1 message with empty sender name, got %d", len(messages))
	}
	if len(messages) > 0 && messages[0].Subject != "Ghost Message" {
		t.Errorf("expected 'Ghost Message', got %q", messages[0].Subject)
	}
	for _, m := range messages {
		if m.Subject == "Hello World" || m.Subject == "Re: Hello" {
			t.Errorf("should not match message with valid sender: %q", m.Subject)
		}
	}
}

func TestMatchEmptySenderName_MixedFromRecipients(t *testing.T) {
	env := newTestEnv(t)

	// Resolve participant IDs dynamically to avoid coupling to seed order.
	aliceID := env.MustLookupParticipant("alice@example.com")

	nullID := env.AddParticipant(dbtest.ParticipantOpts{Email: nil, DisplayName: nil, Domain: ""})
	env.AddMessage(dbtest.MessageOpts{Subject: "Mixed From", SentAt: "2024-06-01 10:00:00", FromID: aliceID})
	lastMsgID := env.LastMessageID()
	_, err := env.DB.Exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')`, lastMsgID, nullID)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	filter := MessageFilter{EmptyValueTargets: emptyTargets(ViewSenderNames)}
	messages := env.MustListMessages(filter)

	for _, m := range messages {
		if m.Subject == "Mixed From" {
			t.Error("MatchEmptySenderName should not match message with at least one valid from sender")
		}
	}
}

func TestMatchEmptySenderName_CombinedWithDomain(t *testing.T) {
	env := newTestEnvWithEmptyBuckets(t)

	filter := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenderNames),
		Domain:            "example.com",
	}
	messages := env.MustListMessages(filter)

	if len(messages) != 0 {
		t.Errorf("expected 0 messages for MatchEmptySenderName+Domain, got %d", len(messages))
	}
}

func TestGetGmailIDsByFilter_Label(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name    string
		label   string
		wantLen int
	}{
		{"exact_case", "Work", 2},
		{"case_insensitive", "work", 2},
		{"no_match", "Nonexistent", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, err := env.Engine.GetGmailIDsByFilter(
				env.Ctx, MessageFilter{Label: tt.label},
			)
			if err != nil {
				t.Fatalf("GetGmailIDsByFilter: %v", err)
			}
			if len(ids) != tt.wantLen {
				t.Errorf("got %d IDs, want %d", len(ids), tt.wantLen)
			}
		})
	}
}

func TestGetGmailIDsByFilter_SenderName(t *testing.T) {
	env := newTestEnv(t)

	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{SenderName: "Alice Smith"})
	if err != nil {
		t.Fatalf("GetGmailIDsByFilter: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 gmail IDs for Alice Smith, got %d", len(ids))
	}
}

func TestGetGmailIDsByFilter_RecipientName(t *testing.T) {
	env := newTestEnv(t)

	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{RecipientName: "Bob Jones"})
	if err != nil {
		t.Fatalf("GetGmailIDsByFilter: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 gmail IDs for Bob Jones, got %d", len(ids))
	}
}

func TestGetGmailIDsByFilter_RecipientName_WithMatchEmptyRecipient(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{
		RecipientName:     "Bob Jones",
		EmptyValueTargets: emptyTargets(ViewRecipients),
	}
	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, filter)
	if err != nil {
		t.Fatalf("GetGmailIDsByFilter: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 gmail IDs, got %d", len(ids))
	}
}

func TestListMessages_ConversationIDFilter(t *testing.T) {
	env := newTestEnv(t)

	// Resolve participant IDs dynamically to avoid coupling to seed order.
	aliceID := env.MustLookupParticipant("alice@example.com")
	bobID := env.MustLookupParticipant("bob@company.org")

	conv2 := env.AddConversation(dbtest.ConversationOpts{SourceID: 1, Title: "Second Thread"})
	env.AddMessage(dbtest.MessageOpts{
		ConversationID: conv2,
		Subject:        "Thread 2 Message 1",
		SentAt:         "2024-04-01 10:00:00",
		SizeEstimate:   100,
		FromID:         aliceID,
		ToIDs:          []int64{bobID},
	})
	env.AddMessage(dbtest.MessageOpts{
		ConversationID: conv2,
		Subject:        "Thread 2 Message 2",
		SentAt:         "2024-04-02 11:00:00",
		SizeEstimate:   200,
		FromID:         bobID,
		ToIDs:          []int64{aliceID},
	})

	convID1 := int64(1)
	messages1 := env.MustListMessages(MessageFilter{ConversationID: &convID1})
	if len(messages1) != 5 {
		t.Errorf("expected 5 messages in conversation 1, got %d", len(messages1))
	}
	for _, msg := range messages1 {
		if msg.ConversationID != 1 {
			t.Errorf("expected conversation_id=1, got %d for message %d", msg.ConversationID, msg.ID)
		}
	}

	messages2 := env.MustListMessages(MessageFilter{ConversationID: &conv2})
	if len(messages2) != 2 {
		t.Errorf("expected 2 messages in conversation 2, got %d", len(messages2))
	}
	for _, msg := range messages2 {
		if msg.ConversationID != conv2 {
			t.Errorf("expected conversation_id=%d, got %d for message %d", conv2, msg.ConversationID, msg.ID)
		}
	}

	filter2Asc := MessageFilter{
		ConversationID: &conv2,
		Sorting:        MessageSorting{Field: MessageSortByDate, Direction: SortAsc},
	}
	messagesAsc := env.MustListMessages(filter2Asc)
	if len(messagesAsc) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messagesAsc))
	}
	if messagesAsc[0].Subject != "Thread 2 Message 1" {
		t.Errorf("expected first message to be 'Thread 2 Message 1', got %q", messagesAsc[0].Subject)
	}
	if messagesAsc[1].Subject != "Thread 2 Message 2" {
		t.Errorf("expected second message to be 'Thread 2 Message 2', got %q", messagesAsc[1].Subject)
	}
}

// =============================================================================
// MatchEmpty* filter tests (using newTestEnvWithEmptyBuckets)
// =============================================================================

func TestListMessages_MatchEmptyFilters(t *testing.T) {
	env := newTestEnvWithEmptyBuckets(t)

	tests := []struct {
		name      string
		filter    MessageFilter
		wantCount int
		validate  func(*testing.T, []MessageSummary)
	}{
		{
			name:      "Empty sender name",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewSenderNames)},
			wantCount: 1,
			validate: func(t *testing.T, msgs []MessageSummary) {
				if msgs[0].Subject != "No Sender" {
					t.Errorf("expected 'No Sender', got %q", msgs[0].Subject)
				}
			},
		},
		{
			name:      "Empty sender",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewSenders)},
			wantCount: 1,
			validate: func(t *testing.T, msgs []MessageSummary) {
				if msgs[0].Subject != "No Sender" {
					t.Errorf("expected 'No Sender' message, got %q", msgs[0].Subject)
				}
			},
		},
		{
			name:      "Empty recipient",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewRecipients)},
			wantCount: 2,
		},
		{
			name:      "Empty domain",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewDomains)},
			wantCount: 2,
		},
		{
			name:      "Empty label",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewLabels)},
			wantCount: 4,
		},
		{
			name:      "Empty label combined with sender",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewLabels), Sender: "alice@example.com"},
			wantCount: 2,
			validate: func(t *testing.T, msgs []MessageSummary) {
				subjects := make(map[string]bool)
				for _, m := range msgs {
					subjects[m.Subject] = true
				}
				if !subjects["No Labels"] {
					t.Error("expected 'No Labels' message")
				}
				if !subjects["No Recipients"] {
					t.Error("expected 'No Recipients' message")
				}
			},
		},
		{
			name:   "Empty recipient name includes no-recipients message",
			filter: MessageFilter{EmptyValueTargets: emptyTargets(ViewRecipientNames)},
			validate: func(t *testing.T, msgs []MessageSummary) {
				if len(msgs) == 0 {
					t.Fatal("expected at least 1 message with empty recipient name, got 0")
				}
				found := false
				for _, m := range msgs {
					if m.Subject == "No Recipients" {
						found = true
					}
				}
				if !found {
					t.Errorf("expected 'No Recipients' message in results")
				}
			},
		},
		{
			name:      "EmptyValueTarget=ViewSenders alone",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewSenders)},
			wantCount: 1,
			validate: func(t *testing.T, msgs []MessageSummary) {
				if msgs[0].Subject != "No Sender" {
					t.Errorf("expected 'No Sender' message, got %q", msgs[0].Subject)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := env.MustListMessages(tt.filter)
			if tt.wantCount > 0 && len(messages) != tt.wantCount {
				t.Fatalf("got %d messages, want %d", len(messages), tt.wantCount)
			}
			if tt.validate != nil {
				tt.validate(t, messages)
			}
		})
	}
}

func TestRecipientAndRecipientNameAndMatchEmptyRecipient(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{
		Recipient:         "bob@company.org",
		RecipientName:     "Bob Jones",
		EmptyValueTargets: emptyTargets(ViewRecipients),
	}

	messages := env.MustListMessages(filter)
	if len(messages) != 3 {
		t.Errorf("ListMessages: expected 3 messages, got %d", len(messages))
	}

	rows, err := env.Engine.SubAggregate(env.Ctx, filter, ViewSenders, AggregateOptions{Limit: 100})
	if err != nil {
		t.Fatalf("SubAggregate: %v", err)
	}
	if len(rows) != 1 || rows[0].Key != "alice@example.com" {
		t.Errorf("SubAggregate: unexpected rows: %v", rows)
	}
}

// TestRecipientNameFilter_IncludesBCC verifies that RecipientName filter includes BCC recipients.
// Regression test for a bug where RecipientName only searched 'to' and 'cc' but not 'bcc'.
func TestRecipientNameFilter_IncludesBCC(t *testing.T) {
	env := newTestEnv(t)

	sp := dbtest.StrPtr
	aliceID := env.AddParticipant(dbtest.ParticipantOpts{Email: sp("alice-bcc@example.com"), DisplayName: sp("Alice Sender"), Domain: "example.com"})
	secretID := env.AddParticipant(dbtest.ParticipantOpts{Email: sp("secret@example.com"), DisplayName: sp("Secret Bob"), Domain: "example.com"})
	bobID := env.MustLookupParticipant("bob@company.org")

	env.AddMessage(dbtest.MessageOpts{
		Subject: "BCC Test Subject",
		SentAt:  "2024-01-15 10:00:00",
		FromID:  aliceID,
		ToIDs:   []int64{bobID},
		BccIDs:  []int64{secretID},
	})

	t.Run("ListMessages", func(t *testing.T) {
		messages := env.MustListMessages(MessageFilter{RecipientName: "Secret Bob"})
		if len(messages) != 1 {
			t.Errorf("expected 1 message, got %d", len(messages))
		}
	})

	t.Run("AggregateByRecipientName", func(t *testing.T) {
		rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, AggregateOptions{Limit: 100})
		if err != nil {
			t.Fatalf("AggregateByRecipientName: %v", err)
		}
		found := false
		for _, row := range rows {
			if row.Key == "Secret Bob" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected BCC recipient 'Secret Bob' in aggregate, got: %v", rows)
		}
	})

	t.Run("SubAggregate", func(t *testing.T) {
		rows, err := env.Engine.SubAggregate(env.Ctx, MessageFilter{RecipientName: "Secret Bob"}, ViewSenders, AggregateOptions{Limit: 100})
		if err != nil {
			t.Fatalf("SubAggregate: %v", err)
		}
		if len(rows) != 1 || rows[0].Key != "alice-bcc@example.com" {
			t.Errorf("expected sender alice-bcc@example.com, got: %v", rows)
		}
	})

	t.Run("GetGmailIDsByFilter", func(t *testing.T) {
		ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{RecipientName: "Secret Bob"})
		if err != nil {
			t.Fatalf("GetGmailIDsByFilter: %v", err)
		}
		if len(ids) != 1 {
			t.Errorf("expected 1 gmail ID, got: %v", ids)
		}
	})

	t.Run("Recipient_email_also_finds_BCC", func(t *testing.T) {
		messages := env.MustListMessages(MessageFilter{Recipient: "secret@example.com"})
		if len(messages) != 1 {
			t.Errorf("expected 1 message, got %d", len(messages))
		}
	})
}

// TestMultipleEmptyTargets verifies that drilling from one empty bucket into another
// preserves both empty constraints. This tests the fix for the bug where
// EmptyValueTarget could only hold one dimension.
func TestMultipleEmptyTargets(t *testing.T) {
	env := newTestEnvWithEmptyBuckets(t)

	// Scenario: User drills into "empty sender names" then into "empty labels".
	// The filter should find messages that have BOTH empty sender name AND no labels.
	filter := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenderNames, ViewLabels),
	}

	messages := env.MustListMessages(filter)

	// From the test fixture, "No Sender" has no sender name AND no labels.
	// It should be the only message matching both constraints.
	if len(messages) != 1 {
		t.Errorf("expected 1 message matching both empty sender name AND empty labels, got %d", len(messages))
		for _, m := range messages {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	if len(messages) == 1 && messages[0].Subject != "No Sender" {
		t.Errorf("expected 'No Sender', got %q", messages[0].Subject)
	}

	// Test another constraint: empty senders AND empty recipients.
	// "No Sender" has no FromID AND no ToIDs, so it matches both constraints.
	filter2 := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenders, ViewRecipients),
	}

	messages2 := env.MustListMessages(filter2)

	// "No Sender" has BOTH empty sender AND empty recipients
	if len(messages2) != 1 {
		t.Errorf("expected 1 message matching both empty senders AND empty recipients, got %d", len(messages2))
		for _, m := range messages2 {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	if len(messages2) == 1 && messages2[0].Subject != "No Sender" {
		t.Errorf("expected 'No Sender', got %q", messages2[0].Subject)
	}

	// Test constraint: empty recipients AND empty labels.
	// From the fixture, none of the added empty-bucket messages have labels,
	// so both "No Sender" (no recipients, no labels) and "No Recipients" (no recipients, no labels) match.
	filter3 := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewRecipients, ViewLabels),
	}

	messages3 := env.MustListMessages(filter3)

	// Both "No Sender" and "No Recipients" have no recipients AND no labels
	if len(messages3) != 2 {
		t.Errorf("expected 2 messages matching empty recipients AND empty labels, got %d", len(messages3))
		for _, m := range messages3 {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	// Verify the subjects - order may vary
	subjects := make(map[string]bool)
	for _, m := range messages3 {
		subjects[m.Subject] = true
	}
	if !subjects["No Sender"] || !subjects["No Recipients"] {
		t.Errorf("expected both 'No Sender' and 'No Recipients', got %v", subjects)
	}

	// Test truly exclusive constraint: combine empty senders with a specific label
	// "No Sender" has no sender but also no labels, so combining with Label should return 0
	filter4 := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenders),
		Label:             "INBOX",
	}

	messages4 := env.MustListMessages(filter4)

	// No message has both empty sender AND label INBOX
	if len(messages4) != 0 {
		t.Errorf("expected 0 messages matching empty senders AND label INBOX, got %d", len(messages4))
		for _, m := range messages4 {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	// Also test via SubAggregate: drilling from empty senders + labels into domains view
	rows, err := env.Engine.SubAggregate(env.Ctx, filter, ViewDomains, DefaultAggregateOptions())
	if err != nil {
		t.Fatalf("SubAggregate with multiple empty targets: %v", err)
	}

	// "No Sender" has no sender so no domain - expect empty or just the empty bucket
	// Since it has no sender, there's no domain to aggregate on
	if len(rows) != 0 {
		t.Errorf("expected 0 domain sub-aggregate rows for no-sender message, got %d", len(rows))
	}
}

// TestGetTotalStatsWithSearchQuery verifies that GetTotalStats filters stats
// to reflect only messages matching the search query. This is a regression test
// for a bug where SQLiteEngine.GetTotalStats ignored opts.SearchQuery, returning
// global stats instead of search-filtered stats.
func TestGetTotalStatsWithSearchQuery(t *testing.T) {
	env := newTestEnv(t)

	// Without search: 5 messages total
	allStats := env.MustGetTotalStats(StatsOptions{})
	if allStats.MessageCount != 5 {
		t.Fatalf("expected 5 total messages, got %d", allStats.MessageCount)
	}

	// Search "Hello" matches 2 messages: "Hello World" (id=1, size=1000, no att)
	// and "Re: Hello" (id=2, size=2000, 2 attachments: 10000+5000 bytes).
	stats := env.MustGetTotalStats(StatsOptions{SearchQuery: "Hello"})

	if stats.MessageCount != 2 {
		t.Errorf("SearchQuery=Hello: expected 2 messages, got %d", stats.MessageCount)
	}
	expectedSize := int64(1000 + 2000)
	if stats.TotalSize != expectedSize {
		t.Errorf("SearchQuery=Hello: expected total size %d, got %d", expectedSize, stats.TotalSize)
	}
	if stats.AttachmentCount != 2 {
		t.Errorf("SearchQuery=Hello: expected 2 attachments, got %d", stats.AttachmentCount)
	}
	expectedAttSize := int64(10000 + 5000)
	if stats.AttachmentSize != expectedAttSize {
		t.Errorf("SearchQuery=Hello: expected attachment size %d, got %d", expectedAttSize, stats.AttachmentSize)
	}
}

// TestGetTotalStatsWithSearchQuery_FromFilter verifies that from: search
// filters are applied correctly to stats.
func TestGetTotalStatsWithSearchQuery_FromFilter(t *testing.T) {
	env := newTestEnv(t)

	// "from:alice" should match 3 messages (ids 1,2,3)
	stats := env.MustGetTotalStats(StatsOptions{SearchQuery: "from:alice@example.com"})

	if stats.MessageCount != 3 {
		t.Errorf("SearchQuery=from:alice: expected 3 messages, got %d", stats.MessageCount)
	}
	expectedSize := int64(1000 + 2000 + 1500)
	if stats.TotalSize != expectedSize {
		t.Errorf("SearchQuery=from:alice: expected total size %d, got %d", expectedSize, stats.TotalSize)
	}
}

// TestGetTotalStatsWithSearchQuery_Combined verifies that SearchQuery combines
// with other StatsOptions filters (e.g., WithAttachmentsOnly).
func TestGetTotalStatsWithSearchQuery_Combined(t *testing.T) {
	env := newTestEnv(t)

	// "from:alice" matches 3 messages (ids 1,2,3), but only id=2 has attachments.
	stats := env.MustGetTotalStats(StatsOptions{
		SearchQuery:         "from:alice@example.com",
		WithAttachmentsOnly: true,
	})

	if stats.MessageCount != 1 {
		t.Errorf("SearchQuery+WithAttachments: expected 1 message, got %d", stats.MessageCount)
	}
	if stats.TotalSize != 2000 {
		t.Errorf("SearchQuery+WithAttachments: expected total size 2000, got %d", stats.TotalSize)
	}
}

func TestGetMessageRaw(t *testing.T) {
	env := newTestEnv(t)
	rawMIME := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nHello")

	msgID := env.AddMessage(dbtest.MessageOpts{Subject: "Raw Test", SentAt: "2024-06-01 12:00:00"})
	_, err := env.DB.Exec(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression) VALUES (?, ?, 'mime', 'none')`,
		msgID, rawMIME,
	)
	if err != nil {
		t.Fatalf("insert message_raw: %v", err)
	}

	got, err := env.Engine.GetMessageRaw(env.Ctx, msgID)
	if err != nil {
		t.Fatalf("GetMessageRaw: %v", err)
	}
	if !bytes.Equal(got, rawMIME) {
		t.Errorf("GetMessageRaw = %q, want %q", got, rawMIME)
	}
}

func TestGetMessageRaw_NotFound(t *testing.T) {
	env := newTestEnv(t)

	got, err := env.Engine.GetMessageRaw(env.Ctx, 999999)
	if err != nil {
		t.Fatalf("GetMessageRaw unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("GetMessageRaw = %q, want nil", got)
	}
}

// TestGetMessageRaw_FiltersDeletedFromSource verifies that GetMessageRaw
// refuses to serve raw MIME for messages whose deleted_from_source_at is
// set, keeping the raw-MIME endpoint aligned with how list/search hide
// deleted-from-source messages.
func TestGetMessageRaw_FiltersDeletedFromSource(t *testing.T) {
	env := newTestEnv(t)
	rawMIME := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nHello")

	msgID := env.AddMessage(dbtest.MessageOpts{Subject: "Deleted", SentAt: "2024-06-01 12:00:00"})
	if _, err := env.DB.Exec(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression) VALUES (?, ?, 'mime', 'none')`,
		msgID, rawMIME,
	); err != nil {
		t.Fatalf("insert message_raw: %v", err)
	}
	if _, err := env.DB.Exec(
		`UPDATE messages SET deleted_from_source_at = '2024-06-02 12:00:00' WHERE id = ?`,
		msgID,
	); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	got, err := env.Engine.GetMessageRaw(env.Ctx, msgID)
	if err != nil {
		t.Fatalf("GetMessageRaw: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for deleted-from-source message, got %d bytes", len(got))
	}
}

// TestGetMessage_PopulatesDeletedAt verifies that the engine's GetMessage
// surfaces deleted_from_source_at via MessageDetail.DeletedAt so the API
// can include it in detail responses.
func TestGetMessage_PopulatesDeletedAt(t *testing.T) {
	env := newTestEnv(t)

	msgID := env.AddMessage(dbtest.MessageOpts{Subject: "Soft-deleted", SentAt: "2024-06-01 12:00:00"})
	if _, err := env.DB.Exec(
		`UPDATE messages SET deleted_from_source_at = '2024-06-02 12:00:00' WHERE id = ?`,
		msgID,
	); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	msg, err := env.Engine.GetMessage(env.Ctx, msgID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg == nil {
		t.Fatal("GetMessage returned nil for deleted message; expected the message with DeletedAt set")
	}
	if msg.DeletedAt == nil {
		t.Errorf("DeletedAt = nil, want non-nil for deleted message")
	}
}
