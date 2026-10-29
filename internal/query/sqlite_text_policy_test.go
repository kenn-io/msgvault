package query_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSQLiteTextParticipantValuesRequireFullSupport(t *testing.T) {
	for _, tc := range []struct{ name, displayName, email, phone string }{
		{name: "named phone", displayName: "Supported Contact", phone: "+15550000123"},
		{name: "named email", displayName: "Supported Contact", email: "shared@example.test"},
		{name: "phone fallback", phone: "+15550000123"},
		{name: "email fallback", email: "shared@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := testutil.NewSQLiteTestStore(t)
			source, err := s.GetOrCreateSource("slack", "synthetic:text-policy")
			require.NoError(err)
			support, err := s.EnsureConversation(source.ID, "support", "Support room")
			require.NoError(err)
			target, err := s.EnsureConversation(source.ID, "target", "Target room")
			require.NoError(err)
			for _, room := range []string{"support", "target"} {
				require.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, room, conversationpolicy.Full))
			}
			rich, err := s.ResolveConversationParticipantContext(t.Context(), support, store.ConversationParticipantIdentity{
				IdentifierType: "slack", IdentifierValue: "SYNTHETIC:SHARED", DisplayName: tc.displayName, Email: tc.email, Phone: tc.phone,
			})
			require.NoError(err)
			bare, err := s.ResolveConversationParticipantContext(t.Context(), target, store.ConversationParticipantIdentity{
				IdentifierType: "slack", IdentifierValue: "SYNTHETIC:SHARED",
			})
			require.NoError(err)
			require.Equal(rich.ParticipantID, bare.ParticipantID)
			id, err := s.UpsertMessage(&store.Message{
				SourceID: source.ID, ConversationID: target, SourceMessageID: "target-message", MessageType: "slack",
				SenderID: sql.NullInt64{Int64: rich.ParticipantID, Valid: true},
				Subject:  sql.NullString{String: "needle", Valid: true}, Snippet: sql.NullString{String: "needle", Valid: true},
				SentAt: sql.NullTime{Time: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), Valid: true},
			})
			require.NoError(err)
			_, err = s.BackfillFTS(nil)
			require.NoError(err)
			engine := query.NewSQLiteEngine(s.DB())
			before, err := engine.TextSearch(t.Context(), "needle", nil, 10, 0)
			require.NoError(err)
			require.Len(before, 1)
			if tc.displayName != "" {
				assert.Equal(tc.displayName, before[0].FromName)
			}
			assert.Equal(tc.email, before[0].FromEmail)
			assert.Equal(tc.phone, before[0].FromPhone)
			contact := tc.phone
			if contact == "" {
				contact = tc.email
			}
			name := tc.displayName
			if name == "" {
				name = contact
			}
			for _, group := range []struct {
				view query.TextViewType
				key  string
			}{{query.TextViewContacts, contact}, {query.TextViewContactNames, name}} {
				rows, err := engine.TextAggregate(t.Context(), group.view, query.TextAggregateOptions{})
				require.NoError(err)
				require.Len(rows, 1)
				assert.Equal(group.key, rows[0].Key)
				assert.Equal(int64(1), rows[0].Count)
			}
			for _, filter := range []query.TextFilter{{ContactPhone: contact}, {ContactName: name}} {
				rows, err := engine.ListConversations(t.Context(), filter)
				require.NoError(err)
				require.Len(rows, 1)
				assert.Equal(target, rows[0].ConversationID)
			}
			require.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "support", conversationpolicy.Cold))

			for _, view := range []query.TextViewType{query.TextViewContacts, query.TextViewContactNames} {
				rows, err := engine.TextAggregate(t.Context(), view, query.TextAggregateOptions{})
				require.NoError(err)
				assert.Empty(rows, "view %v must omit identities supported only by Cold history", view)
			}
			for _, filter := range []query.TextFilter{
				{ContactPhone: "+15550000123"}, {ContactPhone: "shared@example.test"},
				{ContactName: "Supported Contact"}, {ContactName: "+15550000123"}, {ContactName: "shared@example.test"},
			} {
				rows, err := engine.ListConversations(t.Context(), filter)
				require.NoError(err)
				assert.Empty(rows, "filter %+v must not match Cold-only participant values", filter)
			}
			assertRedacted := func(rows []query.MessageSummary, err error) {
				t.Helper()
				require.NoError(err)
				require.Len(rows, 1, "Full-room message must remain accessible")
				assert.Equal(id, rows[0].ID)
				assert.Empty(rows[0].FromName)
				assert.Empty(rows[0].FromEmail)
				assert.Empty(rows[0].FromPhone)
			}
			assertRedacted(engine.TextSearch(t.Context(), "needle", nil, 10, 0))
			assertRedacted(engine.ListConversationMessages(t.Context(), target, query.TextFilter{}))
			snapshot, _, err := engine.ListConversationMessagesSnapshot(t.Context(), target, query.TextFilter{})
			assertRedacted(snapshot, err)
		})
	}
}
