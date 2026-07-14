package query

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

type rebindRecordingDialect struct {
	Dialect

	queries []string
}

func (d *rebindRecordingDialect) Rebind(query string) string {
	rebound := d.Dialect.Rebind(query)
	d.queries = append(d.queries, rebound)
	return rebound
}

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
	assert := assert.New(t)
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

	assert.Equal(t, "bob@example.com", clone.Sender)
	assert.Nil(t, clone.EmptyValueTargets)

	// Mutating clone should not affect original
	clone.SetEmptyTarget(ViewSenders)
	assert.Nil(t, original.EmptyValueTargets, "original EmptyValueTargets should still be nil")
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
			assert.Equal(t, tt.want, tt.filter.HasEmptyTargets())
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
			filter:    MessageFilter{SenderName: "Alice"},
			wantCount: 3,
		},
		{
			name:      "Filter by recipient name",
			filter:    MessageFilter{RecipientName: "Bob"},
			wantCount: 3,
		},
		{
			name:      "Combined recipient and recipient name",
			filter:    MessageFilter{Recipient: "bob@company.org", RecipientName: "Bob"},
			wantCount: 3,
		},
		{
			name:      "Mismatched recipient and recipient name",
			filter:    MessageFilter{Recipient: "bob@company.org", RecipientName: "Alice"},
			wantCount: 0,
		},
		{
			name:      "RecipientName with MatchEmptyRecipient (contradictory)",
			filter:    MessageFilter{RecipientName: "Bob", EmptyValueTargets: emptyTargets(ViewRecipients)},
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
			assert.Len(t, messages, tt.wantCount)
			if tt.validate != nil {
				tt.validate(t, messages)
			}
		})
	}
}

func TestListMessages_NoDuplicates(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{Recipient: "bob@company.org", RecipientName: "Bob"}
	messages := env.MustListMessages(filter)

	seen := make(map[int64]int)
	for _, m := range messages {
		seen[m.ID]++
	}
	for id, count := range seen {
		assert.LessOrEqual(t, count, 1, "message ID %d returned %d times (expected once)", id, count)
	}
}

func TestListMessagesWithLabels(t *testing.T) {
	env := newTestEnv(t)

	messages := env.MustListMessages(MessageFilter{})

	msg1 := messages[len(messages)-1]
	assert.Len(t, msg1.Labels, 2, "msg1 labels")
}

func TestGetMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
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

func TestGetMessageWithURLBackedAttachment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)

	_, err := env.DB.Exec(`
		INSERT INTO attachments (message_id, filename, mime_type, size, content_hash, storage_path)
		VALUES (1, 'deck.pptx', 'reference', 0, '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 'https://sp/deck.pptx')
	`)
	require.NoError(err, "insert URL-backed attachment")

	msg, err := env.Engine.GetMessage(env.Ctx, 1)
	require.NoError(err, "GetMessage")
	require.Len(msg.Attachments, 1)
	assert.Equal("deck.pptx", msg.Attachments[0].Filename)
	assert.Empty(msg.Attachments[0].ContentHash)
	assert.Equal("https://sp/deck.pptx", msg.Attachments[0].URL)
}

func TestGetAttachmentClearsURLBackedContentHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)

	result, err := env.DB.Exec(`
		INSERT INTO attachments (message_id, filename, mime_type, size, content_hash, storage_path)
		VALUES (1, 'recording.mp4', 'video/mp4', 0, 'abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd', 'https://sp/recording.mp4')
	`)
	require.NoError(err, "insert URL-backed attachment")
	attID, err := result.LastInsertId()
	require.NoError(err, "LastInsertId")

	att, err := env.Engine.GetAttachment(env.Ctx, attID)
	require.NoError(err, "GetAttachment")
	require.NotNil(att)
	assert.Empty(att.ContentHash)
	assert.Equal("https://sp/recording.mp4", att.URL)
}

func TestGetAttachmentsByHashUsesDialectRebind(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	_, err := env.DB.Exec(`
		INSERT INTO attachments (message_id, filename, mime_type, size, content_hash, storage_path)
		VALUES (1, 'report.pdf', 'application/pdf', 123, ?, '01/report.pdf')
	`, hash)
	require.NoError(err, "insert attachment")

	dialect := &rebindRecordingDialect{Dialect: PostgreSQLQueryDialect{}}
	engine := NewEngineWithDialect(env.DB, dialect)
	attachments, err := engine.GetAttachmentsByHash(env.Ctx, hash)
	require.NoError(err, "GetAttachmentsByHash")
	require.Len(attachments, 1, "attachments")
	require.NotEmpty(dialect.queries, "dialect Rebind calls")
	assert.Contains(dialect.queries[len(dialect.queries)-1], "content_hash = $1", "rebound query")
}

func TestGetMessageBySourceID(t *testing.T) {
	env := newTestEnv(t)

	msg, err := env.Engine.GetMessageBySourceID(env.Ctx, "msg3")
	require.NoError(t, err, "GetMessageBySourceID")
	require.NotNil(t, msg, "expected message")
	assert.Equal(t, "Follow up", msg.Subject)
}

func TestListAccounts(t *testing.T) {
	env := newTestEnv(t)

	accounts, err := env.Engine.ListAccounts(env.Ctx)
	require.NoError(t, err, "ListAccounts")
	require.Len(t, accounts, 1)
	assert.Equal(t, "test@gmail.com", accounts[0].Identifier)
}

func TestGetTotalStats(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	stats := env.MustGetTotalStats(StatsOptions{})

	assert.Equal(int64(5), stats.MessageCount, "MessageCount")
	assert.Equal(int64(3), stats.AttachmentCount, "AttachmentCount")
	assert.Equal(int64(1000+2000+1500+3000+500), stats.TotalSize, "TotalSize")
	assert.Equal(int64(10000+5000+20000), stats.AttachmentSize, "AttachmentSize")
}

func TestGetTotalStatsSourceDeletedBreakdown(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	// One of the five seeded messages is deleted from its source account
	// but retained in the archive.
	env.MarkDeletedBySourceID("msg3")

	// Default: the archive is the system of record, so the total includes
	// the source-deleted message and the breakdown splits it out.
	stats := env.MustGetTotalStats(StatsOptions{})
	assert.Equal(int64(5), stats.MessageCount, "MessageCount includes source-deleted")
	assert.Equal(int64(4), stats.ActiveMessageCount, "ActiveMessageCount")
	assert.Equal(int64(1), stats.SourceDeletedMessageCount, "SourceDeletedMessageCount")

	// hide_deleted excludes the source-deleted message from every field.
	hidden := env.MustGetTotalStats(StatsOptions{HideDeletedFromSource: true})
	assert.Equal(int64(4), hidden.MessageCount, "MessageCount with hide_deleted")
	assert.Equal(int64(4), hidden.ActiveMessageCount, "ActiveMessageCount with hide_deleted")
	assert.Equal(int64(0), hidden.SourceDeletedMessageCount, "SourceDeletedMessageCount with hide_deleted")
}

// TestListMessagesFromNameUsesPerMessageDisplayName verifies SQLite message
// summaries hydrate FromName from the message's own "from" recipient
// display_name (the per-message Gmail "From: Name <...>" override), matching
// the name sender-name aggregation buckets by — not the participant's sticky
// display_name. Otherwise drilling into a per-message sender-name bucket shows
// a different name than the bucket it came from.
func TestListMessagesFromNameUsesPerMessageDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)

	email := "sender@example.com"
	sticky := "Sticky Participant Name"
	senderID := env.AddParticipant(dbtest.ParticipantOpts{
		Email: &email, DisplayName: &sticky, Domain: "example.com",
	})
	msgID := env.AddMessage(dbtest.MessageOpts{
		Subject: "per-message name test",
		FromID:  senderID,
	})
	env.SetFromName(msgID, "Per Message Override")

	msgs := env.MustListMessages(MessageFilter{})
	var got *MessageSummary
	for i := range msgs {
		if msgs[i].ID == msgID {
			got = &msgs[i]
			break
		}
	}
	require.NotNil(got, "message %d in list", msgID)
	assert.Equal("Per Message Override", got.FromName,
		"FromName must reflect the per-message from display_name, not the sticky participant name")
	assert.Equal(email, got.FromEmail, "FromEmail still from the participant")
}

// TestGetTextStatsSourceDeletedBreakdown verifies GetTextStats populates the
// active/source-deleted breakdown alongside the total message count, so
// /api/v1/text/stats reports non-zero breakdown fields.
func TestGetTextStatsSourceDeletedBreakdown(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	// Seed three text-type (SMS) messages; mark one deleted from its source.
	env.AddMessage(dbtest.MessageOpts{Subject: "sms one", MessageType: "sms", SizeEstimate: 100})
	env.AddMessage(dbtest.MessageOpts{Subject: "sms two", MessageType: "sms", SizeEstimate: 100})
	deletedID := env.AddMessage(dbtest.MessageOpts{Subject: "sms three", MessageType: "sms", SizeEstimate: 100})
	env.MarkDeletedBySourceID(fmt.Sprintf("msg%d", deletedID))

	// A dedup-hidden row (deleted_at IS NOT NULL) must be excluded from
	// every breakdown, matching the other read paths.
	dedupID := env.AddMessage(dbtest.MessageOpts{Subject: "sms dup", MessageType: "sms", SizeEstimate: 100})
	env.MarkDedupLoserByID(dedupID)

	stats := env.MustGetTextStats(TextStatsOptions{})
	assert.Equal(int64(3), stats.MessageCount, "MessageCount excludes dedup-hidden, includes source-deleted")
	assert.Equal(int64(2), stats.ActiveMessageCount, "ActiveMessageCount")
	assert.Equal(int64(1), stats.SourceDeletedMessageCount, "SourceDeletedMessageCount")
}

func TestGetTotalStatsWithSourceID(t *testing.T) {
	assert := assert.New(t)
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
	assert := assert.New(t)
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
	assert.Equal(t, int64(5), allStats.MessageCount, "total messages")

	attStats := env.MustGetTotalStats(StatsOptions{WithAttachmentsOnly: true})
	assert.Equal(t, int64(2), attStats.MessageCount, "messages with attachments")
	assert.NotZero(t, attStats.AttachmentCount, "non-zero attachment count for messages with attachments")
}

func TestHideDeletedFromSourceSearchFast(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	assert.Equal(t, int64(5), allStats.MessageCount, "total messages")

	// Mark message 1 as deleted
	env.MarkDeletedByID(1)

	// Without HideDeletedFromSource: still 5
	stats := env.MustGetTotalStats(StatsOptions{})
	assert.Equal(t, int64(5), stats.MessageCount, "messages (deleted included)")

	// With HideDeletedFromSource: 4
	hiddenStats := env.MustGetTotalStats(StatsOptions{HideDeletedFromSource: true})
	assert.Equal(t, int64(4), hiddenStats.MessageCount, "messages (deleted hidden)")
}

func TestDeletedMessagesIncludedWithFlag(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	env.MarkDeletedByID(1)

	rows, err := env.Engine.Aggregate(env.Ctx, ViewSenders, DefaultAggregateOptions())
	require.NoError(t, err, "Aggregate(ViewSenders)")
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
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
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
	assert := assert.New(t)
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
	require.NoError(t, err, "insert")

	filter := MessageFilter{EmptyValueTargets: emptyTargets(ViewSenderNames)}
	messages := env.MustListMessages(filter)

	for _, m := range messages {
		assert.NotEqual(t, "Mixed From", m.Subject,
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

	assert.Empty(t, messages, "expected 0 messages for MatchEmptySenderName+Domain")
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
			require.NoError(t, err, "GetGmailIDsByFilter")
			assert.Len(t, ids, tt.wantLen)
		})
	}
}

func TestGetGmailIDsByFilter_SenderName(t *testing.T) {
	env := newTestEnv(t)

	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{SenderName: "Alice"})
	require.NoError(t, err, "GetGmailIDsByFilter")
	assert.Len(t, ids, 3, "expected 3 gmail IDs for Alice")
}

func TestGetGmailIDsByFilter_AfterBefore(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	env := newTestEnv(t)
	feb1 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	mar1 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	afterIDs, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{After: &feb1})
	require.NoError(err, "after-only")
	assert.ElementsMatch([]string{"msg3", "msg4", "msg5"}, afterIDs, "after >= Feb 1 (boundary inclusive)")

	beforeIDs, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{Before: &feb1})
	require.NoError(err, "before-only")
	assert.ElementsMatch([]string{"msg1", "msg2"}, beforeIDs, "before < Feb 1 (boundary exclusive)")

	rangeIDs, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{After: &feb1, Before: &mar1})
	require.NoError(err, "range")
	assert.ElementsMatch([]string{"msg3", "msg4"}, rangeIDs, "Feb window")

	combined, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{Sender: "alice@example.com", After: &feb1})
	require.NoError(err, "combined with sender")
	assert.ElementsMatch([]string{"msg3"}, combined, "sender+after")
}

// addMultiAuthorMessage inserts a message with TWO distinct 'from' rows so the
// queried email lives on one row and the queried display name on the other.
// It returns the message subject. Used to prove that combining a Sender (email)
// filter with a SenderName filter binds BOTH to the SAME from-row rather than
// matching across different authors of a multi-author message.
func addMultiAuthorMessage(env *testEnv, subject string) string {
	authorAID := env.AddParticipant(dbtest.ParticipantOpts{
		Email:       new("author-a@example.com"),
		DisplayName: new("Author A"),
		Domain:      "example.com",
	})
	authorBID := env.AddParticipant(dbtest.ParticipantOpts{
		Email:       new("author-b@example.com"),
		DisplayName: new("Author B"),
		Domain:      "example.com",
	})

	// AddMessage's FromID inserts the first 'from' row (Author A); add the
	// second 'from' row (Author B) manually so the message has two authors.
	msgID := env.AddMessage(dbtest.MessageOpts{Subject: subject, SentAt: "2024-06-10 10:00:00", FromID: authorAID})
	_, err := env.DB.Exec(
		`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')`,
		msgID, authorBID,
	)
	require.NoError(env.T, err, "insert second from row")
	return subject
}

// TestListMessages_SenderEmailAndName_SameFromRow asserts that when BOTH the
// Sender (email) and SenderName filters are set, they must match the SAME
// from-row of a multi-author message — a cross-row match (email on Author A,
// name on Author B) must NOT match, while a same-row match still does.
func TestListMessages_SenderEmailAndName_SameFromRow(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	subject := addMultiAuthorMessage(env, "Two Authors")

	// Cross-row: email on Author A's row, name on Author B's row. The pre-fix
	// builder emitted two independent EXISTS and matched; the fix must not.
	crossRow := env.MustListMessages(MessageFilter{
		Sender:     "author-a@example.com",
		SenderName: "Author B",
	})
	for _, m := range crossRow {
		assert.NotEqual(subject, m.Subject,
			"cross-row sender email+name must not match a multi-author message")
	}

	// Same-row: email and name both on Author A's row — still matches.
	sameRow := env.MustListMessages(MessageFilter{
		Sender:     "author-a@example.com",
		SenderName: "Author A",
	})
	assert.True(slices.ContainsFunc(sameRow, func(m MessageSummary) bool { return m.Subject == subject }),
		"same-row sender email+name must still match")
}

// TestGetGmailIDsByFilter_SenderEmailAndName_SameFromRow mirrors
// TestListMessages_SenderEmailAndName_SameFromRow for the GetGmailIDsByFilter
// builder site (deletion/staging path).
func TestGetGmailIDsByFilter_SenderEmailAndName_SameFromRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)

	addMultiAuthorMessage(env, "Two Authors GID")
	crossRowMsgID := env.LastMessageID()
	crossRowGmailID := fmt.Sprintf("msg%d", crossRowMsgID)

	crossRow, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{
		Sender:     "author-a@example.com",
		SenderName: "Author B",
	})
	require.NoError(err, "GetGmailIDsByFilter cross-row")
	assert.NotContains(crossRow, crossRowGmailID,
		"cross-row sender email+name must not match a multi-author message")

	sameRow, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{
		Sender:     "author-a@example.com",
		SenderName: "Author A",
	})
	require.NoError(err, "GetGmailIDsByFilter same-row")
	assert.Contains(sameRow, crossRowGmailID,
		"same-row sender email+name must still match")
}

// addMultiRecipientMessage inserts a message with TWO distinct 'to' rows so the
// queried recipient email lives on one row and the queried display name on the
// other. It returns the message subject. Used to prove that combining a
// Recipient (email) filter with a RecipientName filter binds BOTH to the SAME
// to/cc/bcc row rather than matching across different recipients of a
// multi-recipient message.
func addMultiRecipientMessage(env *testEnv, subject string) string {
	recipAID := env.AddParticipant(dbtest.ParticipantOpts{
		Email:       new("recip-a@example.com"),
		DisplayName: new("Recip A"),
		Domain:      "example.com",
	})
	recipBID := env.AddParticipant(dbtest.ParticipantOpts{
		Email:       new("recip-b@example.com"),
		DisplayName: new("Recip B"),
		Domain:      "example.com",
	})

	env.AddMessage(dbtest.MessageOpts{Subject: subject, SentAt: "2024-06-10 10:00:00", ToIDs: []int64{recipAID, recipBID}})
	return subject
}

// TestListMessages_RecipientEmailAndName_SameToRow asserts that when BOTH the
// Recipient (email) and RecipientName filters are set, they must match the SAME
// to/cc/bcc row of a multi-recipient message — a cross-row match (email on
// Recip A, name on Recip B) must NOT match, while a same-row match still does.
func TestListMessages_RecipientEmailAndName_SameToRow(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	subject := addMultiRecipientMessage(env, "Two Recipients")

	// Cross-row: email on Recip A's row, name on Recip B's row. The pre-fix
	// builder emitted two independent EXISTS and matched; the fix must not.
	crossRow := env.MustListMessages(MessageFilter{
		Recipient:     "recip-a@example.com",
		RecipientName: "Recip B",
	})
	for _, m := range crossRow {
		assert.NotEqual(subject, m.Subject,
			"cross-row recipient email+name must not match a multi-recipient message")
	}

	// Same-row: email and name both on Recip A's row — still matches.
	sameRow := env.MustListMessages(MessageFilter{
		Recipient:     "recip-a@example.com",
		RecipientName: "Recip A",
	})
	assert.True(slices.ContainsFunc(sameRow, func(m MessageSummary) bool { return m.Subject == subject }),
		"same-row recipient email+name must still match")
}

// TestGetGmailIDsByFilter_RecipientEmailAndName_SameToRow mirrors
// TestListMessages_RecipientEmailAndName_SameToRow for the GetGmailIDsByFilter
// builder site (deletion/staging path).
func TestGetGmailIDsByFilter_RecipientEmailAndName_SameToRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)

	addMultiRecipientMessage(env, "Two Recipients GID")
	crossRowMsgID := env.LastMessageID()
	crossRowGmailID := fmt.Sprintf("msg%d", crossRowMsgID)

	crossRow, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{
		Recipient:     "recip-a@example.com",
		RecipientName: "Recip B",
	})
	require.NoError(err, "GetGmailIDsByFilter cross-row")
	assert.NotContains(crossRow, crossRowGmailID,
		"cross-row recipient email+name must not match a multi-recipient message")

	sameRow, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{
		Recipient:     "recip-a@example.com",
		RecipientName: "Recip A",
	})
	require.NoError(err, "GetGmailIDsByFilter same-row")
	assert.Contains(sameRow, crossRowGmailID,
		"same-row recipient email+name must still match")
}

func TestGetGmailIDsByFilter_RecipientName(t *testing.T) {
	env := newTestEnv(t)

	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{RecipientName: "Bob"})
	require.NoError(t, err, "GetGmailIDsByFilter")
	assert.Len(t, ids, 3, "expected 3 gmail IDs for Bob")
}

func TestGetGmailIDsByFilter_RecipientName_WithMatchEmptyRecipient(t *testing.T) {
	env := newTestEnv(t)

	filter := MessageFilter{
		RecipientName:     "Bob",
		EmptyValueTargets: emptyTargets(ViewRecipients),
	}
	ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, filter)
	require.NoError(t, err, "GetGmailIDsByFilter")
	assert.Len(t, ids, 3, "expected 3 gmail IDs")
}

func TestListMessages_ConversationIDFilter(t *testing.T) {
	assert := assert.New(t)
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
	require.Len(t, messagesAsc, 2)
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
				t.Helper()
				assert.Equal(t, "No Sender", msgs[0].Subject)
			},
		},
		{
			name:      "Empty sender",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewSenders)},
			wantCount: 1,
			validate: func(t *testing.T, msgs []MessageSummary) {
				t.Helper()
				assert.Equal(t, "No Sender", msgs[0].Subject)
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
				t.Helper()
				subjects := make(map[string]bool)
				for _, m := range msgs {
					subjects[m.Subject] = true
				}
				assert.True(t, subjects["No Labels"], "expected 'No Labels' message")
				assert.True(t, subjects["No Recipients"], "expected 'No Recipients' message")
			},
		},
		{
			name:   "Empty recipient name includes no-recipients message",
			filter: MessageFilter{EmptyValueTargets: emptyTargets(ViewRecipientNames)},
			validate: func(t *testing.T, msgs []MessageSummary) {
				t.Helper()
				require.NotEmpty(t, msgs, "expected at least 1 message with empty recipient name")
				found := false
				for _, m := range msgs {
					if m.Subject == "No Recipients" {
						found = true
					}
				}
				assert.True(t, found, "expected 'No Recipients' message in results")
			},
		},
		{
			name:      "EmptyValueTarget=ViewSenders alone",
			filter:    MessageFilter{EmptyValueTargets: emptyTargets(ViewSenders)},
			wantCount: 1,
			validate: func(t *testing.T, msgs []MessageSummary) {
				t.Helper()
				assert.Equal(t, "No Sender", msgs[0].Subject)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := env.MustListMessages(tt.filter)
			if tt.wantCount > 0 {
				require.Len(t, messages, tt.wantCount)
			}
			if tt.validate != nil {
				tt.validate(t, messages)
			}
		})
	}
}

func TestRecipientAndRecipientNameAndMatchEmptyRecipient(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)

	filter := MessageFilter{
		Recipient:         "bob@company.org",
		RecipientName:     "Bob",
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
		assert.Len(t, messages, 1)
	})

	t.Run("AggregateByRecipientName", func(t *testing.T) {
		rows, err := env.Engine.Aggregate(env.Ctx, ViewRecipientNames, AggregateOptions{Limit: 100})
		require.NoError(t, err, "AggregateByRecipientName")
		found := false
		for _, row := range rows {
			if row.Key == "Secret Bob" {
				found = true
				break
			}
		}
		assert.True(t, found, "expected BCC recipient 'Secret Bob' in aggregate, got: %v", rows)
	})

	t.Run("SubAggregate", func(t *testing.T) {
		rows, err := env.Engine.SubAggregate(env.Ctx, MessageFilter{RecipientName: "Secret Bob"}, ViewSenders, AggregateOptions{Limit: 100})
		require.NoError(t, err, "SubAggregate")
		require.Len(t, rows, 1)
		assert.Equal(t, "alice-bcc@example.com", rows[0].Key)
	})

	t.Run("GetGmailIDsByFilter", func(t *testing.T) {
		ids, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{RecipientName: "Secret Bob"})
		require.NoError(t, err, "GetGmailIDsByFilter")
		assert.Len(t, ids, 1)
	})

	t.Run("Recipient_email_also_finds_BCC", func(t *testing.T) {
		messages := env.MustListMessages(MessageFilter{Recipient: "secret@example.com"})
		assert.Len(t, messages, 1)
	})
}

// TestMultipleEmptyTargets verifies that drilling from one empty bucket into another
// preserves both empty constraints. This tests the fix for the bug where
// EmptyValueTarget could only hold one dimension.
func TestMultipleEmptyTargets(t *testing.T) {
	assert := assert.New(t)
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
	require.NoError(t, err, "SubAggregate with multiple empty targets")

	// "No Sender" has no sender so no domain - expect empty or just the empty bucket
	// Since it has no sender, there's no domain to aggregate on
	assert.Empty(rows, "domain sub-aggregate rows for no-sender message")
}

// TestGetTotalStatsWithSearchQuery verifies that GetTotalStats filters stats
// to reflect only messages matching the search query. This is a regression test
// for a bug where SQLiteEngine.GetTotalStats ignored opts.SearchQuery, returning
// global stats instead of search-filtered stats.
func TestGetTotalStatsWithSearchQuery(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	// Without search: 5 messages total
	allStats := env.MustGetTotalStats(StatsOptions{})
	require.Equal(t, int64(5), allStats.MessageCount, "total messages")

	// Search "Hello" matches 2 messages: "Hello World" (id=1, size=1000, no att)
	// and "Re: Hello" (id=2, size=2000, 2 attachments: 10000+5000 bytes).
	stats := env.MustGetTotalStats(StatsOptions{SearchQuery: "Hello"})

	assert.Equal(int64(2), stats.MessageCount, "SearchQuery=Hello messages")
	assert.Equal(int64(1000+2000), stats.TotalSize, "SearchQuery=Hello total size")
	assert.Equal(int64(2), stats.AttachmentCount, "SearchQuery=Hello attachments")
	assert.Equal(int64(10000+5000), stats.AttachmentSize, "SearchQuery=Hello attachment size")
}

func TestGetTotalStats_SearchScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)

	meetingID := env.AddMessage(dbtest.MessageOpts{
		Subject:      "cross-type stats needle",
		MessageType:  messageTypeMeetingTranscript,
		SizeEstimate: 420,
		SentAt:       "2024-04-01 10:00:00",
	})

	defaultStats := env.MustGetTotalStats(StatsOptions{})
	assert.Equal(int64(5), defaultStats.MessageCount, "default analytics remain email-only")
	assert.Equal(int64(8000), defaultStats.TotalSize, "default analytics size")

	defaultSearchStats := env.MustGetTotalStats(StatsOptions{SearchQuery: "cross-type stats needle"})
	assert.Zero(defaultSearchStats.MessageCount, "ordinary analytics search excludes meetings")

	searchStats := env.MustGetTotalStats(StatsOptions{
		SearchQuery: "cross-type stats needle",
		SearchScope: true,
	})
	assert.Equal(int64(1), searchStats.MessageCount, "search-scope message count")
	assert.Equal(int64(420), searchStats.TotalSize, "search-scope total size")

	searchResult, err := env.Engine.SearchFastWithStats(
		env.Ctx,
		search.Parse("cross-type stats needle"),
		"cross-type stats needle",
		MessageFilter{},
		ViewSenders,
		100,
		0,
	)
	require.NoError(err, "SearchFastWithStats")
	require.NotNil(searchResult.Stats, "SearchFastWithStats stats")
	require.Len(searchResult.Messages, 1, "SearchFastWithStats messages")
	assert.Equal(meetingID, searchResult.Messages[0].ID, "meeting search result")
	assert.Equal(searchResult.TotalCount, searchStats.MessageCount, "search/stats count agreement")
	assert.Equal(searchResult.Stats.TotalSize, searchStats.TotalSize, "search/stats size agreement")
}

func TestSQLiteMessageSummariesIncludeSourceID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	sourceID := env.AddSource(dbtest.SourceOpts{Identifier: "meeting-source"})
	conversationID := env.AddConversation(dbtest.ConversationOpts{
		SourceID: sourceID, Title: "Meeting",
	})
	messageID := env.AddMessage(dbtest.MessageOpts{
		SourceID:       sourceID,
		ConversationID: conversationID,
		Subject:        "sourceidentityneedle",
		MessageType:    messageTypeMeetingTranscript,
	})

	listed, err := env.Engine.ListMessages(env.Ctx, MessageFilter{MessageType: messageTypeMeetingTranscript})
	require.NoError(err, "ListMessages")
	require.Len(listed, 1)
	assert.Equal(sourceID, listed[0].SourceID)

	searched, err := env.Engine.Search(
		env.Ctx,
		search.Parse("message_type:meeting_transcript sourceidentityneedle"),
		10,
		0,
	)
	require.NoError(err, "Search")
	require.Len(searched, 1)
	assert.Equal(sourceID, searched[0].SourceID)

	hydrated, err := env.Engine.GetMessageSummariesByIDs(env.Ctx, []int64{messageID})
	require.NoError(err, "GetMessageSummariesByIDs")
	require.Len(hydrated, 1)
	assert.Equal(sourceID, hydrated[0].SourceID)

	detail, err := env.Engine.GetMessage(env.Ctx, messageID)
	require.NoError(err, "GetMessage")
	require.NotNil(detail)
	assert.Equal(sourceID, detail.SourceID)
}

func TestGetTotalStats_SearchScopeCountsMatchingLabelsAndSources(t *testing.T) {
	env := newTestEnv(t)
	source2 := env.AddSource(dbtest.SourceOpts{
		Identifier: "second@example.com", DisplayName: "Second Account",
	})
	source3 := env.AddSource(dbtest.SourceOpts{
		Identifier: "third@example.com", DisplayName: "Third Account",
	})
	conversation2 := env.AddConversation(dbtest.ConversationOpts{
		SourceID: source2, Title: "Second Conversation",
	})
	conversation3 := env.AddConversation(dbtest.ConversationOpts{
		SourceID: source3, Title: "Third Conversation",
	})
	label1A := env.AddLabel(dbtest.LabelOpts{SourceID: 1, Name: "Scoped First A"})
	label1B := env.AddLabel(dbtest.LabelOpts{SourceID: 1, Name: "Scoped First B"})
	label2 := env.AddLabel(dbtest.LabelOpts{SourceID: source2, Name: "Scoped Second"})
	label3 := env.AddLabel(dbtest.LabelOpts{SourceID: source3, Name: "Scoped Third"})
	message1 := env.AddMessage(dbtest.MessageOpts{
		Subject: "ordinary first subject", MessageType: messageTypeMeetingTranscript,
		SizeEstimate: 110, HasAttachments: true,
	})
	message2 := env.AddMessage(dbtest.MessageOpts{
		SourceID: source2, ConversationID: conversation2,
		Subject: "ordinary second subject", MessageType: messageTypeMeetingTranscript, SizeEstimate: 220,
	})
	message3 := env.AddMessage(dbtest.MessageOpts{
		SourceID: source3, ConversationID: conversation3,
		Subject: "ordinary third subject", MessageType: messageTypeMeetingTranscript,
		SizeEstimate: 330, HasAttachments: true,
	})
	env.AddMessageLabel(message1, label1A)
	env.AddMessageLabel(message1, label1B)
	env.AddMessageLabel(message2, label2)
	env.AddMessageLabel(message3, label3)
	for _, fixture := range []struct {
		messageID int64
		body      string
	}{
		{messageID: message1, body: "scopedbodyneedle appears only in the first body"},
		{messageID: message2, body: "ordinary nonmatching second body"},
		{messageID: message3, body: "scopedbodyneedle appears only in the third body"},
	} {
		_, err := env.DB.Exec(
			`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`,
			fixture.messageID, fixture.body,
		)
		require.NoError(t, err, "insert searchable body")
	}
	for _, fixture := range []struct {
		messageID int64
		size      int64
		path      string
	}{
		{messageID: message1, size: 11, path: "01/first"},
		{messageID: message3, size: 33, path: "03/third"},
	} {
		_, err := env.DB.Exec(`
			INSERT INTO attachments (message_id, filename, mime_type, size, storage_path)
			VALUES (?, 'transcript.txt', 'text/plain', ?, ?)
		`, fixture.messageID, fixture.size, fixture.path)
		require.NoError(t, err, "insert attachment")
	}
	env.EnableFTS()

	engines := []struct {
		name   string
		engine Engine
	}{
		{name: "sqlite", engine: env.Engine},
		{name: "duckdb sqlite delegation", engine: &DuckDBEngine{sqliteEngine: env.Engine}},
	}
	for _, tc := range engines {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			stats, err := tc.engine.GetTotalStats(env.Ctx, StatsOptions{
				SearchQuery: "scopedbodyneedle",
				SearchScope: true,
				SourceIDs:   []int64{1, source2},
			})
			require.NoError(err)
			assert.Equal(int64(1), stats.MessageCount, "matching messages")
			assert.Equal(int64(110), stats.TotalSize, "matching message size")
			assert.Equal(int64(1), stats.AttachmentCount, "matching attachments")
			assert.Equal(int64(11), stats.AttachmentSize, "matching attachment size")
			assert.Equal(int64(2), stats.LabelCount, "labels on matching messages")
			assert.Equal(int64(1), stats.AccountCount, "sources containing matching messages")

			stats, err = tc.engine.GetTotalStats(env.Ctx, StatsOptions{
				SourceID:    &source2,
				SourceIDs:   []int64{1, source3},
				SearchQuery: "scopedbodyneedle",
				SearchScope: true,
			})
			require.NoError(err)
			assert.Equal(int64(2), stats.MessageCount, "multi-source scope overrides single source")
			assert.Equal(int64(440), stats.TotalSize)
			assert.Equal(int64(2), stats.AttachmentCount)
			assert.Equal(int64(44), stats.AttachmentSize)
			assert.Equal(int64(3), stats.LabelCount)
			assert.Equal(int64(2), stats.AccountCount)

			source1 := int64(1)
			emptyStats, err := tc.engine.GetTotalStats(env.Ctx, StatsOptions{
				SearchQuery: "scopedbodyneedle",
				SearchScope: true,
				SourceID:    &source1,
				SourceIDs:   []int64{},
			})
			require.NoError(err)
			assert.Zero(emptyStats.MessageCount)
			assert.Zero(emptyStats.TotalSize)
			assert.Zero(emptyStats.AttachmentCount)
			assert.Zero(emptyStats.AttachmentSize)
			assert.Zero(emptyStats.LabelCount)
			assert.Zero(emptyStats.AccountCount)
		})
	}
}

func TestGetTotalStats_FilteredCountsMatchDuckDBPopulation(t *testing.T) {
	env := newTestEnv(t)
	source2 := env.AddSource(dbtest.SourceOpts{Identifier: "second@example.com"})
	source3 := env.AddSource(dbtest.SourceOpts{Identifier: "third@example.com"})
	conversation2 := env.AddConversation(dbtest.ConversationOpts{SourceID: source2, Title: "Second"})
	conversation3 := env.AddConversation(dbtest.ConversationOpts{SourceID: source3, Title: "Third"})
	label1A := env.AddLabel(dbtest.LabelOpts{SourceID: 1, Name: "Filtered First A"})
	label1B := env.AddLabel(dbtest.LabelOpts{SourceID: 1, Name: "Filtered First B"})
	label2 := env.AddLabel(dbtest.LabelOpts{SourceID: source2, Name: "Filtered Second"})
	label3 := env.AddLabel(dbtest.LabelOpts{SourceID: source3, Name: "Filtered Third"})
	message1 := env.AddMessage(dbtest.MessageOpts{Subject: "filterpopulationneedle first"})
	message2 := env.AddMessage(dbtest.MessageOpts{
		SourceID: source2, ConversationID: conversation2, Subject: "ordinary second",
	})
	message3 := env.AddMessage(dbtest.MessageOpts{
		SourceID: source3, ConversationID: conversation3, Subject: "filterpopulationneedle third",
	})
	env.AddMessageLabel(message1, label1A)
	env.AddMessageLabel(message1, label1B)
	env.AddMessageLabel(message2, label2)
	env.AddMessageLabel(message3, label3)

	builder := NewTestDataBuilder(t)
	duckSource1 := builder.AddSource("first@example.com")
	duckSource2 := builder.AddSource("second@example.com")
	duckSource3 := builder.AddSource("third@example.com")
	duckLabel1A := builder.AddLabel("Filtered First A")
	duckLabel1B := builder.AddLabel("Filtered First B")
	duckLabel2 := builder.AddLabel("Filtered Second")
	duckLabel3 := builder.AddLabel("Filtered Third")
	duckMessage1 := builder.AddMessage(MessageOpt{SourceID: duckSource1, Subject: "filterpopulationneedle first"})
	duckMessage2 := builder.AddMessage(MessageOpt{SourceID: duckSource2, Subject: "ordinary second"})
	duckMessage3 := builder.AddMessage(MessageOpt{SourceID: duckSource3, Subject: "filterpopulationneedle third"})
	builder.AddMessageLabel(duckMessage1, duckLabel1A)
	builder.AddMessageLabel(duckMessage1, duckLabel1B)
	builder.AddMessageLabel(duckMessage2, duckLabel2)
	builder.AddMessageLabel(duckMessage3, duckLabel3)
	builder.SetEmptyAttachments()
	duckEngine := builder.BuildEngine()

	tests := []struct {
		name         string
		opts         StatsOptions
		wantMessages int64
		wantLabels   int64
		wantAccounts int64
	}{
		{
			name:         "default-scope search query",
			opts:         StatsOptions{SearchQuery: "filterpopulationneedle"},
			wantMessages: 2, wantLabels: 3, wantAccounts: 2,
		},
		{
			name:         "default-scope source IDs",
			opts:         StatsOptions{SourceIDs: []int64{source2}},
			wantMessages: 1, wantLabels: 1, wantAccounts: 1,
		},
		{
			name: "explicit empty source IDs",
			opts: StatsOptions{SourceIDs: []int64{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			sqliteStats, err := env.Engine.GetTotalStats(env.Ctx, tc.opts)
			require.NoError(err)
			duckStats, err := duckEngine.GetTotalStats(env.Ctx, tc.opts)
			require.NoError(err)

			for name, stats := range map[string]*TotalStats{"sqlite": sqliteStats, "duckdb": duckStats} {
				assert.Equal(tc.wantMessages, stats.MessageCount, name+" messages")
				assert.Equal(tc.wantLabels, stats.LabelCount, name+" labels")
				assert.Equal(tc.wantAccounts, stats.AccountCount, name+" accounts")
			}
		})
	}
}

func TestSearchFastWithStats_SourceIDsMultiAndEmptyScopes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source2 := env.AddSource(dbtest.SourceOpts{Identifier: "second@example.com"})
	source3 := env.AddSource(dbtest.SourceOpts{Identifier: "third@example.com"})
	conversation2 := env.AddConversation(dbtest.ConversationOpts{SourceID: source2, Title: "Second"})
	conversation3 := env.AddConversation(dbtest.ConversationOpts{SourceID: source3, Title: "Third"})
	message1 := env.AddMessage(dbtest.MessageOpts{Subject: "fastsourceneedle first"})
	message2 := env.AddMessage(dbtest.MessageOpts{
		SourceID: source2, ConversationID: conversation2, Subject: "fastsourceneedle second",
	})
	env.AddMessage(dbtest.MessageOpts{
		SourceID: source3, ConversationID: conversation3, Subject: "fastsourceneedle excluded",
	})
	env.EnableFTS()
	q := search.Parse("fastsourceneedle")

	result, err := env.Engine.SearchFastWithStats(env.Ctx, q, "fastsourceneedle",
		MessageFilter{SourceIDs: []int64{1, source2}}, ViewSenders, 50, 0)
	require.NoError(err)
	gotIDs := make([]int64, len(result.Messages))
	for i := range result.Messages {
		gotIDs[i] = result.Messages[i].ID
	}
	assert.ElementsMatch([]int64{message1, message2}, gotIDs)
	assert.Equal(int64(2), result.TotalCount)
	require.NotNil(result.Stats)
	assert.Equal(int64(2), result.Stats.MessageCount)

	empty, err := env.Engine.SearchFastWithStats(env.Ctx, q, "fastsourceneedle",
		MessageFilter{SourceIDs: []int64{}}, ViewSenders, 50, 0)
	require.NoError(err)
	assert.Empty(empty.Messages)
	assert.Zero(empty.TotalCount)
	require.NotNil(empty.Stats)
	assert.Zero(empty.Stats.MessageCount)
}

func TestGetTotalStatsWithSearchQuery_MessageTypeFilter(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	_, err := env.DB.Exec(`UPDATE messages SET message_type = ? WHERE id = ?`, "sms", int64(2))
	require.NoError(t, err, "mark message as sms")

	stats := env.MustGetTotalStats(StatsOptions{SearchQuery: "message_type:sms Hello"})

	assert.Equal(int64(1), stats.MessageCount, "SearchQuery=message_type:sms messages")
	assert.Equal(int64(2000), stats.TotalSize, "SearchQuery=message_type:sms total size")
	assert.Equal(int64(2), stats.AttachmentCount, "SearchQuery=message_type:sms attachments")
	assert.Equal(int64(10000+5000), stats.AttachmentSize, "SearchQuery=message_type:sms attachment size")
}

// TestGetTotalStatsWithSearchQuery_FromFilter verifies that from: search
// filters are applied correctly to stats.
func TestGetTotalStatsWithSearchQuery_FromFilter(t *testing.T) {
	env := newTestEnv(t)

	// "from:alice" should match 3 messages (ids 1,2,3)
	stats := env.MustGetTotalStats(StatsOptions{SearchQuery: "from:alice@example.com"})

	assert.Equal(t, int64(3), stats.MessageCount, "SearchQuery=from:alice messages")
	assert.Equal(t, int64(1000+2000+1500), stats.TotalSize, "SearchQuery=from:alice total size")
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

	assert.Equal(t, int64(1), stats.MessageCount, "SearchQuery+WithAttachments messages")
	assert.Equal(t, int64(2000), stats.TotalSize, "SearchQuery+WithAttachments total size")
}

func TestSearchFastWithStats_MessageTypeStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)

	smsID := env.AddMessage(dbtest.MessageOpts{
		Subject:      "Lunch via SMS",
		SentAt:       "2024-04-01 10:00:00",
		SizeEstimate: 321,
	})
	_, err := env.DB.Exec(`UPDATE messages SET message_type = 'sms' WHERE id = ?`, smsID)
	require.NoError(err, "set sms message_type")

	q := search.Parse("message_type:sms")
	result, err := env.Engine.SearchFastWithStats(env.Ctx, q, "message_type:sms", MessageFilter{}, ViewSenders, 100, 0)
	require.NoError(err, "SearchFastWithStats")
	require.NotNil(result.Stats, "stats")

	require.Len(result.Messages, 1, "messages")
	assert.Equal(smsID, result.Messages[0].ID, "message id")
	assert.Equal(int64(1), result.TotalCount, "total count")
	assert.Equal(int64(1), result.Stats.MessageCount, "stats message count")
	assert.Equal(int64(321), result.Stats.TotalSize, "stats total size")
}

func TestGetMessageRaw(t *testing.T) {
	env := newTestEnv(t)
	rawMIME := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nHello")

	msgID := env.AddMessage(dbtest.MessageOpts{Subject: "Raw Test", SentAt: "2024-06-01 12:00:00"})
	_, err := env.DB.Exec(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression) VALUES (?, ?, 'mime', 'none')`,
		msgID, rawMIME,
	)
	require.NoError(t, err, "insert message_raw")

	got, err := env.Engine.GetMessageRaw(env.Ctx, msgID)
	require.NoError(t, err, "GetMessageRaw")
	assert.Equal(t, rawMIME, got)
}

func TestGetMessageRaw_NotFound(t *testing.T) {
	env := newTestEnv(t)

	got, err := env.Engine.GetMessageRaw(env.Ctx, 999999)
	require.NoError(t, err, "GetMessageRaw unexpected error")
	assert.Nil(t, got)
}

// TestGetMessageRaw_FiltersDeletedFromSource verifies that GetMessageRaw
// refuses to serve raw MIME for messages whose deleted_from_source_at is
// set, keeping the raw-MIME endpoint aligned with how list/search hide
// deleted-from-source messages.
func TestGetMessageRaw_FiltersDeletedFromSource(t *testing.T) {
	require := require.New(t)
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
	assert.Nil(t, got, "expected nil for deleted-from-source message")
}

// TestGetMessage_PopulatesDeletedAt verifies that the engine's GetMessage
// surfaces deleted_from_source_at via MessageDetail.DeletedAt so the API
// can include it in detail responses.
func TestGetMessage_PopulatesDeletedAt(t *testing.T) {
	require := require.New(t)
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
	assert.NotNil(t, msg.DeletedAt, "DeletedAt should be non-nil for deleted message")
}

func TestGetGmailIDsByMessageIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	env := newTestEnv(t)

	// Happy path: two known fixture messages (msg1=id 1, msg2=id 2).
	ids, err := env.Engine.GetGmailIDsByMessageIDs(env.Ctx, []int64{1, 2})
	require.NoError(err, "resolve fixture ids")
	assert.ElementsMatch([]string{"msg1", "msg2"}, ids)

	// Unknown IDs are silently dropped.
	ids, err = env.Engine.GetGmailIDsByMessageIDs(env.Ctx, []int64{1, 999999})
	require.NoError(err, "unknown id")
	assert.ElementsMatch([]string{"msg1"}, ids)

	// Empty input: no query, no results.
	ids, err = env.Engine.GetGmailIDsByMessageIDs(env.Ctx, nil)
	require.NoError(err, "empty input")
	assert.Empty(ids)
}

func TestGetGmailIDsByMessageIDs_ExcludesNonQualifying(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	env := newTestEnv(t)

	// Non-Gmail source message.
	_, err := env.DB.Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (99, 'whatsapp', 'wa@example.com')`)
	require.NoError(err, "insert whatsapp source")
	_, err = env.DB.Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at) VALUES (901, 1, 99, 'wa-1', 'whatsapp', '2024-01-01')`)
	require.NoError(err, "insert whatsapp message")

	// Remote-deleted and dedup-soft-deleted Gmail messages (source 1 = test@gmail.com).
	_, err = env.DB.Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, deleted_from_source_at) VALUES (902, 1, 1, 'gone-1', 'email', '2024-01-02', '2024-06-01')`)
	require.NoError(err, "insert source-deleted message")
	_, err = env.DB.Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, deleted_at) VALUES (903, 1, 1, 'dedup-1', 'email', '2024-01-03', '2024-06-01')`)
	require.NoError(err, "insert dedup-deleted message")

	ids, err := env.Engine.GetGmailIDsByMessageIDs(env.Ctx, []int64{1, 901, 902, 903})
	require.NoError(err, "resolve mixed ids")
	assert.ElementsMatch([]string{"msg1"}, ids, "non-Gmail, source-deleted, and dedup-deleted must be dropped")
}

func TestGetGmailIDsByMessageIDs_LargeSelectionExceedsSingleQueryLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	env := newTestEnv(t)

	// 33k IDs exceed SQLITE_MAX_VARIABLE_NUMBER (32766) as a single IN
	// list, so an unchunked lookup fails with "too many SQL variables".
	// The real IDs sit in the first and last chunk — with a duplicate of
	// the first — so the merge proves cross-chunk newest-first ordering
	// (msg5 is newer than msg1) and input dedupe.
	ids := make([]int64, 0, 33001)
	ids = append(ids, 1)
	for next := int64(1_000_000); len(ids) < 32999; next++ {
		ids = append(ids, next)
	}
	ids = append(ids, 5, 1)

	gmailIDs, err := env.Engine.GetGmailIDsByMessageIDs(env.Ctx, ids)
	require.NoError(err, "large selection must stay under bind-parameter limits")
	assert.Equal([]string{"msg5", "msg1"}, gmailIDs,
		"newest-first order restored across chunks; duplicate input deduped")
}

func TestGetAccountsByGmailIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	env := newTestEnv(t)

	// Fixture messages all belong to the single Gmail source.
	accounts, err := env.Engine.GetAccountsByGmailIDs(env.Ctx, []string{"msg1", "msg2"})
	require.NoError(err, "resolve fixture accounts")
	assert.Equal([]string{"test@gmail.com"}, accounts)

	// Unknown IDs resolve to no account.
	accounts, err = env.Engine.GetAccountsByGmailIDs(env.Ctx, []string{"does-not-exist"})
	require.NoError(err, "unknown id")
	assert.Empty(accounts)

	// Empty input: no query, no results.
	accounts, err = env.Engine.GetAccountsByGmailIDs(env.Ctx, nil)
	require.NoError(err, "empty input")
	assert.Empty(accounts)
}

func TestGetAccountsByGmailIDs_MultipleAndNonQualifying(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	env := newTestEnv(t)

	// Second Gmail source with a live message.
	_, err := env.DB.Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (98, 'gmail', 'second@gmail.com')`)
	require.NoError(err, "insert second gmail source")
	_, err = env.DB.Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at) VALUES (911, 1, 98, 'other-1', 'email', '2024-01-05')`)
	require.NoError(err, "insert second-account message")

	// Non-Gmail source and a source-deleted Gmail message must not
	// contribute accounts.
	_, err = env.DB.Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (99, 'whatsapp', 'wa@example.com')`)
	require.NoError(err, "insert whatsapp source")
	_, err = env.DB.Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at) VALUES (912, 1, 99, 'wa-1', 'whatsapp', '2024-01-06')`)
	require.NoError(err, "insert whatsapp message")
	_, err = env.DB.Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, deleted_from_source_at) VALUES (913, 1, 98, 'gone-1', 'email', '2024-01-07', '2024-06-01')`)
	require.NoError(err, "insert source-deleted message")

	accounts, err := env.Engine.GetAccountsByGmailIDs(env.Ctx, []string{"msg1", "other-1", "wa-1"})
	require.NoError(err, "resolve mixed accounts")
	assert.Equal([]string{"second@gmail.com", "test@gmail.com"}, accounts,
		"both gmail accounts, sorted; whatsapp excluded")

	accounts, err = env.Engine.GetAccountsByGmailIDs(env.Ctx, []string{"gone-1", "wa-1"})
	require.NoError(err, "resolve non-qualifying")
	assert.Empty(accounts, "deleted and non-Gmail messages contribute no account")
}

func TestGetAccountsByGmailIDs_LargeSelectionExceedsSingleQueryLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	env := newTestEnv(t)

	_, err := env.DB.Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (98, 'gmail', 'second@gmail.com')`)
	require.NoError(err, "insert second gmail source")
	_, err = env.DB.Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at) VALUES (911, 1, 98, 'other-1', 'email', '2024-01-05')`)
	require.NoError(err, "insert second-account message")

	// 33k IDs exceed SQLITE_MAX_VARIABLE_NUMBER (32766) as a single IN
	// list, so an unchunked lookup fails with "too many SQL variables".
	// The two real IDs sit in the first and last chunk to exercise the
	// cross-chunk account union.
	ids := make([]string, 0, 33000)
	ids = append(ids, "msg1")
	for len(ids) < 32999 {
		ids = append(ids, fmt.Sprintf("missing-%d", len(ids)))
	}
	ids = append(ids, "other-1")

	accounts, err := env.Engine.GetAccountsByGmailIDs(env.Ctx, ids)
	require.NoError(err, "large selection must stay under bind-parameter limits")
	assert.Equal([]string{"second@gmail.com", "test@gmail.com"}, accounts,
		"accounts from different chunks must be unioned and sorted")
}
