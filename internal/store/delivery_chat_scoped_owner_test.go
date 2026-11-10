package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
)

func TestChatAdmissionScopedBeeperOwner(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sourceFlag bool
	}{
		{name: "source flagged owner", sourceFlag: true},
		{name: "scoped incoming only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := deliveryChatFixture(t)
			owner, err := f.Store.EnsureParticipantByIdentifier("beeper", beeperidentity.Fallback("synthetic-account", "self-id"), "Owner")
			require.NoError(t, err)
			if tc.sourceFlag {
				// Beeper records provider ownership even before account identities exist.
				_, err = f.Store.UpsertMessage(&store.Message{
					SourceID: f.Source.ID, SourceMessageID: "self-authored", ConversationID: f.ConvID,
					MessageType: "beeper", SenderID: sql.NullInt64{Int64: owner, Valid: true}, IsFromMe: true,
				})
				require.NoError(t, err)
			} else {
				require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "self-id", "manual"))
			}
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			person, _, err := f.Store.CreatePersonFromParticipant(peer)
			require.NoError(t, err)
			require.NoError(t, f.Store.ReplaceConversationParticipants(f.ConvID, []store.ConversationParticipantRef{{ParticipantID: owner}, {ParticipantID: peer}}))
			// The incoming-only case has no self-authored archived message; it must
			// resolve ownership from the account-scoped identity instead.
			_, err = f.Store.UpsertMessage(&store.Message{
				SourceID: f.Source.ID, SourceMessageID: "incoming-only", ConversationID: f.ConvID,
				MessageType: "beeper", SenderID: sql.NullInt64{Int64: peer, Valid: true},
			})
			require.NoError(t, err)
			setDelivery(t, f.Store, person.VCardUID, "", store.DeliverySendAllowed)
			recipients, err := f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "chat-thread")
			require.NoError(t, err)
			assert.Equal(t, []store.DeliveryPolicyQuery{{PersonUID: person.VCardUID, RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread"}}, recipients)
			calls := 0
			err = f.Store.WithDeliveryAdmissionContext(t.Context(), recipients, func(context.Context) error { calls++; return nil })
			assert.NoError(t, err)
			assert.Equal(t, 1, calls)
		})
	}
}
