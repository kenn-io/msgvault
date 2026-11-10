package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// These cases catch admission that skips a current member or reuses approval
// across a later person binding. Both database backends run the same fixtures.
func TestChatRouteAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, reason                                 string
		approve, lateBinding, unbound, changedRoster bool
	}{
		{name: "approved", approve: true},
		{name: "draft only", reason: "draft_required"},
		{name: "unbound group member", approve: true, unbound: true, reason: "unresolved_recipient"},
		{name: "approval before binding", approve: true, lateBinding: true, reason: "approval_predates_address"},
		{name: "roster changes after resolution", approve: true, changedRoster: true, reason: "unresolved_recipient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := deliveryChatFixture(t)
			if tc.unbound || tc.changedRoster {
				_, err := f.Store.EnsureConversationWithType(f.Source.ID, "chat-thread", "group_chat", "")
				require.NoError(t, err)
			}
			owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			original := peer
			if tc.lateBinding {
				original = f.EnsureParticipant("original@example.test", "Original", "example.test")
			}
			p, _, err := f.Store.CreatePersonFromParticipant(original)
			require.NoError(t, err)
			require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
			message := f.NewMessage().WithSourceMessageID("owner-message").Build()
			message.SenderID = sql.NullInt64{Int64: owner, Valid: true}
			_, err = f.Store.UpsertMessage(message)
			require.NoError(t, err)
			require.NoError(t, f.Store.ReplaceConversationParticipants(f.ConvID, []store.ConversationParticipantRef{{ParticipantID: owner}, {ParticipantID: peer}}))
			if tc.approve {
				setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
			}
			if tc.lateBinding {
				_, err = f.Store.LinkParticipants(original, peer)
				require.NoError(t, err)
			}
			if tc.unbound {
				require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, f.EnsureParticipant("unbound@example.test", "Unbound", "example.test"), "member"))
			}
			recipients, err := f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "chat-thread")
			require.NoError(t, err)
			wantRecipients := 1
			if tc.unbound {
				wantRecipients++
			}
			require.Len(t, recipients, wantRecipients)
			if tc.unbound {
				assert.Empty(t, recipients[1].PersonUID)
			}
			if tc.changedRoster {
				require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, f.EnsureParticipant("added@example.test", "Added", "example.test"), "member"))
			}
			calls := 0
			receipt, err := f.Store.ExecuteDeliveryContext(t.Context(), recipients, []byte("synthetic draft"), func(context.Context) error { calls++; return nil })
			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, 1, calls)
				assert.Equal(t, "sent", receipt.Code)
			} else {
				var denied *store.DeliveryAdmissionError
				require.ErrorAs(t, err, &denied)
				assert.Equal(t, "draft_required", denied.Code)
				assert.Equal(t, tc.reason, denied.Reason)
				assert.Zero(t, calls)
				assert.Equal(t, []byte("synthetic draft"), receipt.Content)
			}
			if tc.lateBinding {
				// Refreshing the same allow policy must cover the later binding.
				refresh := setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
				require.NotNil(t, refresh.Audit)
				require.NoError(t, f.Store.WithDeliveryAdmissionContext(t.Context(), recipients, func(context.Context) error { calls++; return nil }))
				assert.Equal(t, 1, calls)
			}
		})
	}
	t.Run("malformed route queries", func(t *testing.T) {
		f := deliveryChatFixture(t)
		peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
		p, _, err := f.Store.CreatePersonFromParticipant(peer)
		require.NoError(t, err)
		require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
		setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
		for _, q := range []store.DeliveryPolicyQuery{
			{PersonUID: p.VCardUID, RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread", Endpoint: "peer@example.test"},
			{PersonUID: p.VCardUID, RouteKind: "unsupported", SourceID: f.Source.ID, SourceConversationID: "chat-thread"},
		} {
			calls := 0
			err := f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryPolicyQuery{q}, func(context.Context) error { calls++; return nil })
			require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
			assert.Zero(t, calls)
		}
	})

	t.Run("email thread is not a chat", func(t *testing.T) {
		f := storetest.New(t)
		peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
		p, _, err := f.Store.CreatePersonFromParticipant(peer)
		require.NoError(t, err)
		require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
		setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
		q := store.DeliveryPolicyQuery{PersonUID: p.VCardUID, RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "default-thread"}
		_, err = f.Store.GetDeliveryPolicyContext(t.Context(), q)
		require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
		calls := 0
		require.Error(t, f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryPolicyQuery{q}, func(context.Context) error { calls++; return nil }))
		assert.Zero(t, calls)
	})

	t.Run("missing conversation", func(t *testing.T) {
		f := storetest.New(t)
		p, _ := deliveryPerson(t, f)
		_, err := f.Store.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: p.VCardUID, RouteKind: "chat", SourceID: f.Source.ID})
		require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
		_, err = f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "")
		require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
	})
}

func TestChatDeliveryPolicyReadRequiresCurrentMember(t *testing.T) {
	f := deliveryChatFixture(t)
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(peer)
	require.NoError(t, err)
	require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
	setDelivery(t, f.Store, person.VCardUID, "", store.DeliverySendAllowed)
	q := store.DeliveryPolicyQuery{PersonUID: person.VCardUID, RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread"}
	state, err := f.Store.GetDeliveryPolicyContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, store.DeliverySendAllowed, state.EffectivePolicy)
	other, err := f.Store.GetOrCreateSource("beeper", "other-account")
	require.NoError(t, err)
	wrongSource := q
	wrongSource.SourceID = other.ID
	_, err = f.Store.GetDeliveryPolicyContext(t.Context(), wrongSource)
	require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
	require.NoError(t, f.Store.ReplaceConversationParticipants(f.ConvID, nil))
	_, err = f.Store.GetDeliveryPolicyContext(t.Context(), q)
	require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
}

func deliveryChatFixture(t *testing.T) *storetest.Fixture {
	t.Helper()
	f := storetest.New(t)
	var err error
	f.Source, err = f.Store.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(t, err)
	f.ConvID, err = f.Store.EnsureConversationWithType(f.Source.ID, "chat-thread", "direct_chat", "")
	require.NoError(t, err)
	return f
}
