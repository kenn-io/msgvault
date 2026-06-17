package query

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// TestSQLInjection_InvalidViewType tests that invalid ViewType values are rejected.
// This ensures that even if a malicious or buggy caller passes an out-of-range
// ViewType, it won't result in undefined behavior or SQL injection.
func TestSQLInjection_InvalidViewType(t *testing.T) {
	env := newTestEnv(t)

	// Attempt to aggregate with an invalid ViewType value
	// This simulates a potential attack vector if enum validation is missing
	invalidViewType := ViewType(999)

	_, err := env.Engine.Aggregate(env.Ctx, invalidViewType, DefaultAggregateOptions())
	assertpkg.ErrorContains(t, err, "unsupported view type")
}

// TestSQLInjection_InvalidSortField tests that invalid SortField values are handled safely.
// The sortClause function should validate the SortField is within expected range.
func TestSQLInjection_InvalidSortField(t *testing.T) {
	env := newTestEnv(t)

	// Create options with an invalid SortField value
	opts := DefaultAggregateOptions()
	opts.SortField = SortField(999) // Invalid sort field

	// The current implementation falls through to default, which is unsafe
	// because it allows arbitrary sort field values without explicit validation.
	// After the fix, this should return an error.
	_, err := env.Engine.Aggregate(env.Ctx, ViewSenders, opts)
	requirepkg.ErrorContains(t, err, "unsupported sort field")
}

// TestSQLInjection_InvalidMessageSortField tests that invalid MessageSortField values
// are handled safely in ListMessages.
func TestSQLInjection_InvalidMessageSortField(t *testing.T) {
	env := newTestEnv(t)

	// Create filter with an invalid MessageSortField value
	filter := MessageFilter{
		Sorting: MessageSorting{
			Field:     MessageSortField(999), // Invalid sort field
			Direction: SortAsc,
		},
	}

	// The current implementation falls through to default, which is unsafe.
	// After the fix, this should return an error.
	_, err := env.Engine.ListMessages(env.Ctx, filter)
	requirepkg.ErrorContains(t, err, "unsupported message sort field")
}

// TestSQLInjection_InvalidTimeGranularity tests that invalid TimeGranularity values
// are handled safely when used in queries.
func TestSQLInjection_InvalidTimeGranularity(t *testing.T) {
	env := newTestEnv(t)

	opts := DefaultAggregateOptions()
	opts.TimeGranularity = TimeGranularity(999) // Invalid granularity

	// When aggregating by time with an invalid granularity, should return error
	_, err := env.Engine.Aggregate(env.Ctx, ViewTime, opts)
	requirepkg.ErrorContains(t, err, "unsupported time granularity")
}

// TestSQLInjection_FilterStringsAreSafelyParameterized verifies that filter
// string fields like Sender, Label, etc. are properly parameterized and
// cannot be used for SQL injection.
func TestSQLInjection_FilterStringsAreSafelyParameterized(t *testing.T) {
	env := newTestEnv(t)

	// These SQL injection payloads should be treated as literal strings,
	// not executed as SQL.
	injectionPayloads := []string{
		"'; DROP TABLE messages; --",
		"alice@example.com' OR '1'='1",
		"alice@example.com\" OR \"1\"=\"1",
		"alice@example.com; DELETE FROM messages WHERE '1'='1",
		"alice@example.com UNION SELECT * FROM messages--",
	}

	for _, payload := range injectionPayloads {
		t.Run("Sender_"+payload[:min(20, len(payload))], func(t *testing.T) {
			filter := MessageFilter{Sender: payload}
			// Should not panic or cause SQL error - just return empty results
			msgs, err := env.Engine.ListMessages(env.Ctx, filter)
			requirepkg.NoError(t, err, "unexpected error with payload %q", payload)
			// Should return 0 results (no match), not all messages
			assertpkg.Empty(t, msgs, "expected 0 results for SQL injection payload")
		})

		t.Run("Label_"+payload[:min(20, len(payload))], func(t *testing.T) {
			filter := MessageFilter{Label: payload}
			msgs, err := env.Engine.ListMessages(env.Ctx, filter)
			requirepkg.NoError(t, err, "unexpected error with payload %q", payload)
			assertpkg.Empty(t, msgs, "expected 0 results for SQL injection payload")
		})
	}

	// Verify the database is still intact after all injection attempts
	var count int
	err := env.DB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	requirepkg.NoError(t, err, "failed to count messages after injection tests")
	assertpkg.Equal(t, 5, count, "expected 5 messages in database after injection tests (standard seed data)")
}

func TestListMessagesFiltersByMessageType(t *testing.T) {
	require := requirepkg.New(t)
	env := newTestEnv(t)
	_, err := env.DB.Exec(`
		INSERT INTO participants (id, phone_number, display_name) VALUES (99, '+15551234567', 'SMS Sender');
		INSERT INTO participants (id, phone_number, display_name) VALUES (100, '+15557654321', 'Me');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet, size_estimate, has_attachments, attachment_count)
		VALUES (99, 1, 1, 'sms-99', 'sms', '2024-04-01 08:00:00', '', 'known sms snippet', 17, 0, 0);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (99, 99, 'from');
		INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (99, 100, 'to');
	`)
	require.NoError(err, "insert sms fixture")

	messages := env.MustListMessages(MessageFilter{
		MessageType: "sms",
		Sorting:     MessageSorting{Field: MessageSortByDate, Direction: SortDesc},
	})

	require.Len(messages, 1)
	require.Equal(int64(99), messages[0].ID)
	require.Equal("sms", messages[0].MessageType)
	require.Equal("+15551234567", messages[0].FromPhone, "summary sender phone")
	require.Equal("SMS Sender", messages[0].FromName, "summary sender name")
	require.Len(messages[0].To, 1, "summary To")
	require.Equal("+15557654321", messages[0].To[0].Email)
	require.Equal("Me", messages[0].To[0].Name)

	detail, err := env.Engine.GetMessage(env.Ctx, 99)
	require.NoError(err, "GetMessage phone-only SMS")
	require.Equal("sms", detail.MessageType)
	require.Len(detail.From, 1, "detail From")
	require.Equal("+15551234567", detail.From[0].Email, "detail From phone fallback")
}
