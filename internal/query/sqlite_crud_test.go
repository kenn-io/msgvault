package query

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	assert := assertpkg.New(t)
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
	assert.Equal("alice@example.com", clone.Sender)
	assert.Equal("INBOX", clone.Label)

	// Verify EmptyValueTargets is deeply copied
	assert.True(clone.MatchesEmpty(ViewSenders), "clone should have ViewSenders in EmptyValueTargets")

	// Mutate the clone's map
	clone.SetEmptyTarget(ViewLabels)

	// Verify original is NOT affected
	assert.False(original.MatchesEmpty(ViewLabels), "original should NOT have ViewLabels after mutating clone")

	// Mutate the original's map
	original.SetEmptyTarget(ViewDomains)

	// Verify clone is NOT affected
	assert.False(clone.MatchesEmpty(ViewDomains), "clone should NOT have ViewDomains after mutating original")
}

// TestMessageFilter_Clone_NilMap verifies Clone handles nil EmptyValueTargets.
func TestMessageFilter_Clone_NilMap(t *testing.T) {
	original := MessageFilter{Sender: "bob@example.com"}
	clone := original.Clone()

	assertpkg.Equal(t, "bob@example.com", clone.Sender)
	assertpkg.Nil(t, clone.EmptyValueTargets)

	// Mutating clone should not affect original
	clone.SetEmptyTarget(ViewSenders)
	assertpkg.Nil(t, original.EmptyValueTargets, "original EmptyValueTargets should still be nil")
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
			assertpkg.Equal(t, tt.want, tt.filter.HasEmptyTargets())
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
			assertpkg.Len(t, messages, tt.wantCount)
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
		assertpkg.LessOrEqual(t, count, 1, "message ID %d returned %d times (expected once)", id, count)
	}
}

func TestListMessagesWithLabels(t *testing.T) {
	env := newTestEnv(t)

	messages := env.MustListMessages(MessageFilter{})

	msg1 := messages[len(messages)-1]
	assertpkg.Len(t, msg1.Labels, 2, "msg1 labels")
}

func TestGetMessage(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	msg, err := env.Engine.GetMessage(env.Ctx, 1)
	require.NoError(err, "GetMessage")
	require.NotNil(msg, "expected message")
	assert.Equal("Hello World", msg.Subject)
	require.Len(msg.From, 1, "from list")
	assert.Equal("alice@example.com", msg.From[0].Email)
	assert.Len(msg.To, 2, "recipients")
	assert.Len(msg.Labels, 2, "labels")
	assert.Equal("Message body 1", msg.BodyText)
}

func TestGetMessageWithAttachments(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	msg, err := env.Engine.GetMessage(env.Ctx, 2)
	require.NoError(err, "GetMessage")
	require.Len(msg.Attachments, 2)
	found := false
	for _, att := range msg.Attachments {
		if att.Filename == "doc.pdf" {
			found = true
			assert.Equal("application/pdf", att.MimeType)
			assert.Equal(int64(10000), att.Size)
		}
	}
	assert.True(found, "expected to find doc.pdf attachment")
}

func TestGetMessageBySourceID(t *testing.T) {
	env := newTestEnv(t)

	msg, err := env.Engine.GetMessageBySourceID(env.Ctx, "msg3")
	requirepkg.NoError(t, err, "GetMessageBySourceID")
	requirepkg.NotNil(t, msg, "expected message")
	assertpkg.Equal(t, "Follow up", msg.Subject)
}

func TestListAccounts(t *testing.T) {
	env := newTestEnv(t)

	accounts, err := env.Engine.ListAccounts(env.Ctx)
	requirepkg.NoError(t, err, "ListAccounts")
	requirepkg.Len(t, accounts, 1)
	assertpkg.Equal(t, "test@gmail.com", accounts[0].Identifier)
}

func TestGetTotalStats(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	stats := env.MustGetTotalStats(StatsOptions{})

	assert.Equal(int64(5), stats.MessageCount, "MessageCount")
	assert.Equal(int64(3), stats.AttachmentCount, "AttachmentCount")
	assert.Equal(int64(1000+2000+1500+3000+500), stats.TotalSize, "TotalSize")
	assert.Equal(int64(10000+5000+20000), stats.AttachmentSize, "AttachmentSize")
}

func TestGetTotalStatsWithSourceID(t *testing.T) {
	assert := assertpkg.New(t)
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
	assert.Equal(int64(6), allStats.MessageCount, "total messages")
	assert.Equal(int64(5), allStats.LabelCount, "total labels")
	assert.Equal(int64(2), allStats.AccountCount, "total accounts")

	sourceID := int64(1)
	source1Stats := env.MustGetTotalStats(StatsOptions{SourceID: &sourceID})
	assert.Equal(int64(5), source1Stats.MessageCount, "messages for source 1")
	assert.Equal(int64(3), source1Stats.LabelCount, "labels for source 1")
	assert.Equal(int64(1), source1Stats.AccountCount, "account count when filtering by source")
}

func TestGetTotalStatsWithInvalidSourceID(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	nonExistentID := int64(9999)
	stats := env.MustGetTotalStats(StatsOptions{SourceID: &nonExistentID})

	assert.Equal(int64(0), stats.MessageCount, "MessageCount")
	assert.Equal(int64(0), stats.LabelCount, "LabelCount")
	assert.Equal(int64(0), stats.AccountCount, "AccountCount")
	assert.Equal(int64(0), stats.AttachmentCount, "AttachmentCount")
}

func TestWithAttachmentsOnlyStats(t *testing.T) {
	env := newTestEnv(t)

	allStats := env.MustGetTotalStats(StatsOptions{})
	assertpkg.Equal(t, int64(5), allStats.MessageCount, "total messages")

	attStats := env.MustGetTotalStats(StatsOptions{WithAttachmentsOnly: true})
	assertpkg.Equal(t, int64(2), attStats.MessageCount, "messages with attachments")
	assertpkg.NotZero(t, attStats.AttachmentCount, "non-zero attachment count for messages with attachments")
}

func TestHideDeletedFromSourceSearchFast(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Use an empty query (matches all messages)
	q := &search.Query{}

	// SearchFast without filter: all 5 messages
	all, err := env.Engine.SearchFast(env.Ctx, q, MessageFilter{}, 100, 0)
	require.NoError(err, "SearchFast")
	assert.Len(all, 5, "SearchFast without filter")

	// SearchFast with HideDeletedFromSource: 4 messages
	hidden, err := env.Engine.SearchFast(env.Ctx, q, MessageFilter{HideDeletedFromSource: true}, 100, 0)
	require.NoError(err, "SearchFast(hide-deleted)")
	assert.Len(hidden, 4, "SearchFast with hide-deleted")

	// SearchFastCount must agree
	count, err := env.Engine.SearchFastCount(env.Ctx, q, MessageFilter{HideDeletedFromSource: true})
	require.NoError(err, "SearchFastCount(hide-deleted)")
	assert.Equal(int64(4), count, "SearchFastCount with hide-deleted")
}

func TestHideDeletedFromSourceStats(t *testing.T) {
	env := newTestEnv(t)

	allStats := env.MustGetTotalStats(StatsOptions{})
	assertpkg.Equal(t, int64(5), allStats.MessageCount, "total messages")

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Without HideDeletedFromSource: still 5
	stats := env.MustGetTotalStats(StatsOptions{})
	assertpkg.Equal(t, int64(5), stats.MessageCount, "messages (deleted included)")

	// With HideDeletedFromSource: 4
	hiddenStats := env.MustGetTotalStats(StatsOptions{HideDeletedFromSource: true})
	assertpkg.Equal(t, int64(4), hiddenStats.MessageCount, "messages (deleted hidden)")
}

func TestDeletedMessagesIncludedWithFlag(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	env.MarkDeletedByID(1)

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "Aggregate(ViewSenders)")
	for _, row := range rows {
		if row.Key == "alice@example.com" {
			assert.Equal(int64(3), row.Count, "alice count (including deleted)")
		}
	}

	messages := env.MustListMessages(MessageFilter{})
	assert.Len(messages, 5, "messages (including deleted)")

	var foundDeleted bool
	for _, msg := range messages {
		if msg.ID == 1 {
			assert.NotNil(msg.DeletedAt, "expected DeletedAt to be set for deleted message")
			foundDeleted = true
		} else {
			assert.Nil(msg.DeletedAt, "expected DeletedAt to be nil for non-deleted message %d", msg.ID)
		}
	}
	assert.True(foundDeleted, "deleted message not found in results")

	stats := env.MustGetTotalStats(StatsOptions{})
	assert.Equal(int64(5), stats.MessageCount, "messages in stats (including deleted)")
}

func TestGetMessageIncludesDeleted(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	env.MarkDeletedByID(1)

	msg, err := env.Engine.GetMessage(env.Ctx, 1)
	require.NoError(err, "GetMessage")
	assert.NotNil(msg, "expected deleted message to be returned")

	msg, err = env.Engine.GetMessage(env.Ctx, 2)
	require.NoError(err, "GetMessage")
	assert.NotNil(msg, "expected message")
}

func TestGetMessageBySourceIDIncludesDeleted(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	env.MarkDeletedBySourceID("msg3")

	msg, err := env.Engine.GetMessageBySourceID(env.Ctx, "msg3")
	require.NoError(err, "GetMessageBySourceID")
	assert.NotNil(msg, "expected deleted message to be returned")

	msg, err = env.Engine.GetMessageBySourceID(env.Ctx, "msg2")
	require.NoError(err, "GetMessageBySourceID")
	assert.NotNil(msg, "expected message")
}

func TestListMessages_MatchEmptySenderName_NotExists(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	env.AddMessage(dbtest.MessageOpts{Subject: "Ghost Message", SentAt: "2024-06-01 10:00:00"})

	filter := MessageFilter{EmptyValueTargets: emptyTargets(ViewSenderNames)}
	messages := env.MustListMessages(filter)

	assert.Len(messages, 1, "messages with empty sender name")
	if len(messages) > 0 {
		assert.Equal("Ghost Message", messages[0].Subject)
	}
	for _, m := range messages {
		assert.NotEqual("Hello World", m.Subject, "should not match message with valid sender")
		assert.NotEqual("Re: Hello", m.Subject, "should not match message with valid sender")
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
	requirepkg.NoError(t, err, "insert")

	filter := MessageFilter{EmptyValueTargets: emptyTargets(ViewSenderNames)}
	messages := env.MustListMessages(filter)

	for _, m := range messages {
		assertpkg.NotEqual(t, "Mixed From", m.Subject,
			"MatchEmptySenderName should not match message with at least one valid from sender")
	}
}

func TestMatchEmptySenderName_CombinedWithDomain(t *testing.T) {
	env := newTestEnvWithEmptyBuckets(t)

	filter := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenderNames),
		Domain:            "example.com",
	}
	messages := env.MustListMessages(filter)

	assertpkg.Empty(t, messages, "expected 0 messages for MatchEmptySenderName+Domain")
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
			requirepkg.NoError(t, err, "GetGmailIDsByFilter")
			assertpkg.Len(t, ids, tt.wantLen)
		})
	}
}

func TestGetGmailIDsByFilter_SenderName(t *testing.T) {
	env := newTestEnv(t)

	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{SenderName: "Alice Smith"})
	requirepkg.NoError(t, err, "GetGmailIDsByFilter")
	assertpkg.Len(t, ids, 3, "expected 3 gmail IDs for Alice Smith")
}

func TestGetGmailIDsByFilter_RecipientName(t *testing.T) {
	env := newTestEnv(t)

	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{RecipientName: "Bob Jones"})
	requirepkg.NoError(t, err, "GetGmailIDsByFilter")
	assertpkg.Len(t, ids, 3, "expected 3 gmail IDs for Bob Jones")
}

func TestGetGmailIDsByFilter_RecipientName_WithMatchEmptyRecipient(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{
		RecipientName:     "Bob Jones",
		EmptyValueTargets: emptyTargets(ViewRecipients),
	}
	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, filter)
	requirepkg.NoError(t, err, "GetGmailIDsByFilter")
	assertpkg.Len(t, ids, 3, "expected 3 gmail IDs")
}

func TestListMessages_ConversationIDFilter(t *testing.T) {
	assert := assertpkg.New(t)
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
	assert.Len(messages1, 5, "messages in conversation 1")
	for _, msg := range messages1 {
		assert.Equal(int64(1), msg.ConversationID, "message %d conversation_id", msg.ID)
	}

	messages2 := env.MustListMessages(MessageFilter{ConversationID: &conv2})
	assert.Len(messages2, 2, "messages in conversation 2")
	for _, msg := range messages2 {
		assert.Equal(conv2, msg.ConversationID, "message %d conversation_id", msg.ID)
	}

	filter2Asc := MessageFilter{
		ConversationID: &conv2,
		Sorting:        MessageSorting{Field: MessageSortByDate, Direction: SortAsc},
	}
	messagesAsc := env.MustListMessages(filter2Asc)
	requirepkg.Len(t, messagesAsc, 2)
	assert.Equal("Thread 2 Message 1", messagesAsc[0].Subject, "first message")
	assert.Equal("Thread 2 Message 2", messagesAsc[1].Subject, "second message")
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
				assertpkg.Equal(t, "No Sender", msgs[0].Subject)
			},
		},
		{
			name:      "Empty sender",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewSenders)},
			wantCount: 1,
			validate: func(t *testing.T, msgs []MessageSummary) {
				assertpkg.Equal(t, "No Sender", msgs[0].Subject)
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
				assertpkg.True(t, subjects["No Labels"], "expected 'No Labels' message")
				assertpkg.True(t, subjects["No Recipients"], "expected 'No Recipients' message")
			},
		},
		{
			name:   "Empty recipient name includes no-recipients message",
			filter: MessageFilter{EmptyValueTargets: emptyTargets(ViewRecipientNames)},
			validate: func(t *testing.T, msgs []MessageSummary) {
				requirepkg.NotEmpty(t, msgs, "expected at least 1 message with empty recipient name")
				found := false
				for _, m := range msgs {
					if m.Subject == "No Recipients" {
						found = true
					}
				}
				assertpkg.True(t, found, "expected 'No Recipients' message in results")
			},
		},
		{
			name:      "EmptyValueTarget=ViewSenders alone",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewSenders)},
			wantCount: 1,
			validate: func(t *testing.T, msgs []MessageSummary) {
				assertpkg.Equal(t, "No Sender", msgs[0].Subject)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := env.MustListMessages(tt.filter)
			if tt.wantCount > 0 {
				requirepkg.Len(t, messages, tt.wantCount)
			}
			if tt.validate != nil {
				tt.validate(t, messages)
			}
		})
	}
}

func TestRecipientAndRecipientNameAndMatchEmptyRecipient(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	filter := MessageFilter{
		Recipient:         "bob@company.org",
		RecipientName:     "Bob Jones",
		EmptyValueTargets: emptyTargets(ViewRecipients),
	}

	messages := env.MustListMessages(filter)
	assert.Len(messages, 3, "ListMessages")

	rows, err := env.Engine.SubAggregate(env.Ctx, filter, ViewSenders, AggregateOptions{Limit: 100})
	require.NoError(err, "SubAggregate")
	require.Len(rows, 1)
	assert.Equal("alice@example.com", rows[0].Key)
}

// TestRecipientNameFilter_IncludesBCC verifies that RecipientName filter includes BCC recipients.
// Regression test for a bug where RecipientName only searched 'to' and 'cc' but not 'bcc'.
func TestRecipientNameFilter_IncludesBCC(t *testing.T) {
	env := newTestEnv(t)

	aliceID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("alice-bcc@example.com"), DisplayName: new("Alice Sender"), Domain: "example.com"})
	secretID := env.AddParticipant(dbtest.ParticipantOpts{Email: new("secret@example.com"), DisplayName: new("Secret Bob"), Domain: "example.com"})
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
		assertpkg.Len(t, messages, 1)
	})

	t.Run("AggregateByRecipientName", func(t *testing.T) {
		rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, AggregateOptions{Limit: 100})
		requirepkg.NoError(t, err, "AggregateByRecipientName")
		found := false
		for _, row := range rows {
			if row.Key == "Secret Bob" {
				found = true
				break
			}
		}
		assertpkg.True(t, found, "expected BCC recipient 'Secret Bob' in aggregate, got: %v", rows)
	})

	t.Run("SubAggregate", func(t *testing.T) {
		rows, err := env.Engine.SubAggregate(env.Ctx, MessageFilter{RecipientName: "Secret Bob"}, ViewSenders, AggregateOptions{Limit: 100})
		requirepkg.NoError(t, err, "SubAggregate")
		requirepkg.Len(t, rows, 1)
		assertpkg.Equal(t, "alice-bcc@example.com", rows[0].Key)
	})

	t.Run("GetGmailIDsByFilter", func(t *testing.T) {
		ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{RecipientName: "Secret Bob"})
		requirepkg.NoError(t, err, "GetGmailIDsByFilter")
		assertpkg.Len(t, ids, 1)
	})

	t.Run("Recipient_email_also_finds_BCC", func(t *testing.T) {
		messages := env.MustListMessages(MessageFilter{Recipient: "secret@example.com"})
		assertpkg.Len(t, messages, 1)
	})
}

// TestMultipleEmptyTargets verifies that drilling from one empty bucket into another
// preserves both empty constraints. This tests the fix for the bug where
// EmptyValueTarget could only hold one dimension.
func TestMultipleEmptyTargets(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnvWithEmptyBuckets(t)

	// Scenario: User drills into "empty sender names" then into "empty labels".
	// The filter should find messages that have BOTH empty sender name AND no labels.
	filter := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenderNames, ViewLabels),
	}

	messages := env.MustListMessages(filter)

	// From the test fixture, "No Sender" has no sender name AND no labels.
	// It should be the only message matching both constraints.
	if !assert.Len(messages, 1, "messages matching both empty sender name AND empty labels") {
		for _, m := range messages {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	if len(messages) == 1 {
		assert.Equal("No Sender", messages[0].Subject)
	}

	// Test another constraint: empty senders AND empty recipients.
	// "No Sender" has no FromID AND no ToIDs, so it matches both constraints.
	filter2 := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenders, ViewRecipients),
	}

	messages2 := env.MustListMessages(filter2)

	// "No Sender" has BOTH empty sender AND empty recipients
	if !assert.Len(messages2, 1, "messages matching both empty senders AND empty recipients") {
		for _, m := range messages2 {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	if len(messages2) == 1 {
		assert.Equal("No Sender", messages2[0].Subject)
	}

	// Test constraint: empty recipients AND empty labels.
	// From the fixture, none of the added empty-bucket messages have labels,
	// so both "No Sender" (no recipients, no labels) and "No Recipients" (no recipients, no labels) match.
	filter3 := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewRecipients, ViewLabels),
	}

	messages3 := env.MustListMessages(filter3)

	// Both "No Sender" and "No Recipients" have no recipients AND no labels
	if !assert.Len(messages3, 2, "messages matching empty recipients AND empty labels") {
		for _, m := range messages3 {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	// Verify the subjects - order may vary
	subjects := make(map[string]bool)
	for _, m := range messages3 {
		subjects[m.Subject] = true
	}
	assert.True(subjects["No Sender"], "expected 'No Sender' message")
	assert.True(subjects["No Recipients"], "expected 'No Recipients' message")

	// Test truly exclusive constraint: combine empty senders with a specific label
	// "No Sender" has no sender but also no labels, so combining with Label should return 0
	filter4 := MessageFilter{
		EmptyValueTargets: emptyTargets(ViewSenders),
		Label:             "INBOX",
	}

	messages4 := env.MustListMessages(filter4)

	// No message has both empty sender AND label INBOX
	if !assert.Empty(messages4, "messages matching empty senders AND label INBOX") {
		for _, m := range messages4 {
			t.Logf("  got: id=%d subject=%q", m.ID, m.Subject)
		}
	}

	// Also test via SubAggregate: drilling from empty senders + labels into domains view
	rows, err := env.Engine.SubAggregate(env.Ctx, filter, ViewDomains, DefaultAggregateOptions())
	requirepkg.NoError(t, err, "SubAggregate with multiple empty targets")

	// "No Sender" has no sender so no domain - expect empty or just the empty bucket
	// Since it has no sender, there's no domain to aggregate on
	assert.Empty(rows, "domain sub-aggregate rows for no-sender message")
}

// TestGetTotalStatsWithSearchQuery verifies that GetTotalStats filters stats
// to reflect only messages matching the search query. This is a regression test
// for a bug where SQLiteEngine.GetTotalStats ignored opts.SearchQuery, returning
// global stats instead of search-filtered stats.
func TestGetTotalStatsWithSearchQuery(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	// Without search: 5 messages total
	allStats := env.MustGetTotalStats(StatsOptions{})
	requirepkg.Equal(t, int64(5), allStats.MessageCount, "total messages")

	// Search "Hello" matches 2 messages: "Hello World" (id=1, size=1000, no att)
	// and "Re: Hello" (id=2, size=2000, 2 attachments: 10000+5000 bytes).
	stats := env.MustGetTotalStats(StatsOptions{SearchQuery: "Hello"})

	assert.Equal(int64(2), stats.MessageCount, "SearchQuery=Hello messages")
	assert.Equal(int64(1000+2000), stats.TotalSize, "SearchQuery=Hello total size")
	assert.Equal(int64(2), stats.AttachmentCount, "SearchQuery=Hello attachments")
	assert.Equal(int64(10000+5000), stats.AttachmentSize, "SearchQuery=Hello attachment size")
}

// TestGetTotalStatsWithSearchQuery_FromFilter verifies that from: search
// filters are applied correctly to stats.
func TestGetTotalStatsWithSearchQuery_FromFilter(t *testing.T) {
	env := newTestEnv(t)

	// "from:alice" should match 3 messages (ids 1,2,3)
	stats := env.MustGetTotalStats(StatsOptions{SearchQuery: "from:alice@example.com"})

	assertpkg.Equal(t, int64(3), stats.MessageCount, "SearchQuery=from:alice messages")
	assertpkg.Equal(t, int64(1000+2000+1500), stats.TotalSize, "SearchQuery=from:alice total size")
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

	assertpkg.Equal(t, int64(1), stats.MessageCount, "SearchQuery+WithAttachments messages")
	assertpkg.Equal(t, int64(2000), stats.TotalSize, "SearchQuery+WithAttachments total size")
}

func TestGetMessageRaw(t *testing.T) {
	env := newTestEnv(t)
	rawMIME := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nHello")

	msgID := env.AddMessage(dbtest.MessageOpts{Subject: "Raw Test", SentAt: "2024-06-01 12:00:00"})
	_, err := env.DB.Exec(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression) VALUES (?, ?, 'mime', 'none')`,
		msgID, rawMIME,
	)
	requirepkg.NoError(t, err, "insert message_raw")

	got, err := env.Engine.GetMessageRaw(env.Ctx, msgID)
	requirepkg.NoError(t, err, "GetMessageRaw")
	assertpkg.Equal(t, rawMIME, got)
}

func TestGetMessageRaw_NotFound(t *testing.T) {
	env := newTestEnv(t)

	got, err := env.Engine.GetMessageRaw(env.Ctx, 999999)
	requirepkg.NoError(t, err, "GetMessageRaw unexpected error")
	assertpkg.Nil(t, got)
}

// TestGetMessageRaw_FiltersDeletedFromSource verifies that GetMessageRaw
// refuses to serve raw MIME for messages whose deleted_from_source_at is
// set, keeping the raw-MIME endpoint aligned with how list/search hide
// deleted-from-source messages.
func TestGetMessageRaw_FiltersDeletedFromSource(t *testing.T) {
	require := requirepkg.New(t)
	env := newTestEnv(t)
	rawMIME := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nHello")

	msgID := env.AddMessage(dbtest.MessageOpts{Subject: "Deleted", SentAt: "2024-06-01 12:00:00"})
	_, err := env.DB.Exec(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression) VALUES (?, ?, 'mime', 'none')`,
		msgID, rawMIME,
	)
	require.NoError(err, "insert message_raw")
	_, err = env.DB.Exec(
		`UPDATE messages SET deleted_from_source_at = '2024-06-02 12:00:00' WHERE id = ?`,
		msgID,
	)
	require.NoError(err, "mark deleted")

	got, err := env.Engine.GetMessageRaw(env.Ctx, msgID)
	require.NoError(err, "GetMessageRaw")
	assertpkg.Nil(t, got, "expected nil for deleted-from-source message")
}

// TestGetMessage_PopulatesDeletedAt verifies that the engine's GetMessage
// surfaces deleted_from_source_at via MessageDetail.DeletedAt so the API
// can include it in detail responses.
func TestGetMessage_PopulatesDeletedAt(t *testing.T) {
	require := requirepkg.New(t)
	env := newTestEnv(t)

	msgID := env.AddMessage(dbtest.MessageOpts{Subject: "Soft-deleted", SentAt: "2024-06-01 12:00:00"})
	_, err := env.DB.Exec(
		`UPDATE messages SET deleted_from_source_at = '2024-06-02 12:00:00' WHERE id = ?`,
		msgID,
	)
	require.NoError(err, "mark deleted")

	msg, err := env.Engine.GetMessage(env.Ctx, msgID)
	require.NoError(err, "GetMessage")
	require.NotNil(msg, "GetMessage returned nil for deleted message; expected the message with DeletedAt set")
	assertpkg.NotNil(t, msg.DeletedAt, "DeletedAt should be non-nil for deleted message")
}
