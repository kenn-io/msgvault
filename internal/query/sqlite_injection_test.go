package query

import (
	"strings"
	"testing"
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
	if err == nil {
		t.Error("expected error for invalid ViewType, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported view type") {
		t.Errorf("expected error message to contain 'unsupported view type', got: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected error for invalid SortField, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported sort field") {
		t.Errorf("expected error message to contain 'unsupported sort field', got: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected error for invalid MessageSortField, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported message sort field") {
		t.Errorf("expected error message to contain 'unsupported message sort field', got: %v", err)
	}
}

// TestSQLInjection_InvalidTimeGranularity tests that invalid TimeGranularity values
// are handled safely when used in queries.
func TestSQLInjection_InvalidTimeGranularity(t *testing.T) {
	env := newTestEnv(t)

	opts := DefaultAggregateOptions()
	opts.TimeGranularity = TimeGranularity(999) // Invalid granularity

	// When aggregating by time with an invalid granularity, should return error
	_, err := env.Engine.Aggregate(env.Ctx, ViewTime, opts)
	if err == nil {
		t.Fatal("expected error for invalid TimeGranularity, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported time granularity") {
		t.Errorf("expected error message to contain 'unsupported time granularity', got: %v", err)
	}
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
			if err != nil {
				t.Errorf("unexpected error with payload %q: %v", payload, err)
			}
			// Should return 0 results (no match), not all messages
			if len(msgs) != 0 {
				t.Errorf("expected 0 results for SQL injection payload, got %d", len(msgs))
			}
		})

		t.Run("Label_"+payload[:min(20, len(payload))], func(t *testing.T) {
			filter := MessageFilter{Label: payload}
			msgs, err := env.Engine.ListMessages(env.Ctx, filter)
			if err != nil {
				t.Errorf("unexpected error with payload %q: %v", payload, err)
			}
			if len(msgs) != 0 {
				t.Errorf("expected 0 results for SQL injection payload, got %d", len(msgs))
			}
		})
	}

	// Verify the database is still intact after all injection attempts
	var count int
	err := env.DB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if err != nil {
		t.Fatalf("failed to count messages after injection tests: %v", err)
	}
	if count != 5 { // Standard seed data has 5 messages
		t.Errorf("expected 5 messages in database after injection tests, got %d", count)
	}
}

func TestListMessagesFiltersByMessageType(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.DB.Exec(`
		INSERT INTO participants (id, phone_number, display_name) VALUES (99, '+15551234567', 'SMS Sender');
		INSERT INTO participants (id, phone_number, display_name) VALUES (100, '+15557654321', 'Me');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet, size_estimate, has_attachments, attachment_count)
		VALUES (99, 1, 1, 'sms-99', 'sms', '2024-04-01 08:00:00', '', 'known sms snippet', 17, 0, 0);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (99, 99, 'from');
		INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (99, 100, 'to');
	`); err != nil {
		t.Fatalf("insert sms fixture: %v", err)
	}

	messages := env.MustListMessages(MessageFilter{
		MessageType: "sms",
		Sorting:     MessageSorting{Field: MessageSortByDate, Direction: SortDesc},
	})

	if len(messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(messages))
	}
	if messages[0].ID != 99 {
		t.Fatalf("message id = %d, want 99", messages[0].ID)
	}
	if messages[0].MessageType != "sms" {
		t.Fatalf("message_type = %q, want sms", messages[0].MessageType)
	}
	if messages[0].FromPhone != "+15551234567" || messages[0].FromName != "SMS Sender" {
		t.Fatalf("summary sender = name %q phone %q, want phone-backed SMS sender", messages[0].FromName, messages[0].FromPhone)
	}
	if len(messages[0].To) != 1 || messages[0].To[0].Email != "+15557654321" || messages[0].To[0].Name != "Me" {
		t.Fatalf("summary To = %#v, want phone-backed SMS recipient", messages[0].To)
	}

	detail, err := env.Engine.GetMessage(env.Ctx, 99)
	if err != nil {
		t.Fatalf("GetMessage phone-only SMS: %v", err)
	}
	if detail.MessageType != "sms" {
		t.Fatalf("detail message_type = %q, want sms", detail.MessageType)
	}
	if len(detail.From) != 1 || detail.From[0].Email != "+15551234567" {
		t.Fatalf("detail From = %#v, want phone fallback", detail.From)
	}
}
