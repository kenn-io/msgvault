package query

import (
	"context"
	"testing"

	_ "github.com/marcboeker/go-duckdb"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// buildTextContactsEngine builds a DuckDB engine over a small Parquet dataset
// shaped like real text data: an iMessage source, phone/email participants, and
// messages whose sender is recorded on messages.sender_id (iMessage/SMS shape)
// or only on a message_recipients row of type 'from' (Messenger shape). It also
// includes an email message that must be excluded from text aggregates.
func buildTextContactsEngine(t *testing.T) *DuckDBEngine {
	t.Helper()
	b := NewTestDataBuilder(t)

	smsSrc := b.AddSourceWithType("me@imessage.local", "imessage")
	emailSrc := b.AddSourceWithType("me@gmail.com", "gmail")

	alice := b.AddPhoneParticipant("+14155550001", "Alice")
	bob := b.AddPhoneParticipant("+14155550002", "Bob")
	emailSender := b.AddParticipant("carol@example.com", "example.com", "Carol")

	// Two iMessages from Alice, sender on messages.sender_id (direct shape).
	m1 := b.AddMessage(MessageOpt{MessageType: "imessage", SourceID: smsSrc, SenderID: &alice})
	m2 := b.AddMessage(MessageOpt{MessageType: "imessage", SourceID: smsSrc, SenderID: &alice})
	b.AddFrom(m1, alice, "Alice")
	b.AddFrom(m2, alice, "Alice")

	// One SMS from Bob, sender recorded ONLY via the 'from' recipient row
	// (no messages.sender_id) — exercises the COALESCE fallback.
	m3 := b.AddMessage(MessageOpt{MessageType: "sms", SourceID: smsSrc})
	b.AddFrom(m3, bob, "Bob")

	// One email — must NOT appear in the text contacts aggregate.
	m4 := b.AddMessage(MessageOpt{MessageType: "email", SourceID: emailSrc, SenderID: &emailSender})
	b.AddFrom(m4, emailSender, "Carol")

	return b.BuildEngine()
}

// TestDuckDBTextAggregate_Contacts guards the DuckDB text Contacts aggregate.
// A prior implementation embedded a correlated scalar subquery in the JOIN ON
// clause, which DuckDB could not optimize over a large messages dataset; this
// asserts the view returns the expected non-email contacts.
func TestDuckDBTextAggregate_Contacts(t *testing.T) {
	engine := buildTextContactsEngine(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		view     TextViewType
		wantKeys map[string]int64 // key -> message count
		absent   string           // key that must not appear (the email sender)
	}{
		{
			name: "contacts (phone/email key)",
			view: TextViewContacts,
			wantKeys: map[string]int64{
				"+14155550001": 2, // Alice, via sender_id
				"+14155550002": 1, // Bob, via 'from' recipient fallback
			},
			absent: "carol@example.com",
		},
		{
			name: "contact names (display name key)",
			view: TextViewContactNames,
			wantKeys: map[string]int64{
				"Alice": 2,
				"Bob":   1,
			},
			absent: "Carol",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)

			rows, err := engine.TextAggregate(ctx, tc.view, TextAggregateOptions{})
			require.NoError(err)
			require.NotEmpty(rows, "contacts aggregate must not be empty")

			got := make(map[string]int64, len(rows))
			for _, r := range rows {
				got[r.Key] = r.Count
			}
			for key, count := range tc.wantKeys {
				assert.Equal(count, got[key], "count for %q", key)
			}
			_, present := got[tc.absent]
			assert.False(present, "email sender %q must be excluded from text aggregate", tc.absent)
		})
	}
}
