package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// These cases catch admission that skips a current member or reuses approval
// across a later person binding. Both database backends run the same fixtures.
func TestChatRouteAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, reason                                 string
		approve, lateBinding, unbound, changedRoster bool
		scopedOwner, selfEvidence                    bool
	}{
		{name: "approved", approve: true, scopedOwner: true},
		{name: "incoming self evidence", approve: true, selfEvidence: true},
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
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			original := peer
			if tc.lateBinding {
				original = f.EnsureParticipant("original@example.test", "Original", "example.test")
			}
			p, _, err := f.Store.CreatePersonFromParticipant(original)
			require.NoError(t, err)
			var owner int64
			if tc.scopedOwner {
				owner, err = f.Store.EnsureParticipantByIdentifier("beeper", beeperidentity.Fallback("synthetic-account", "self-id"), "Owner")
				require.NoError(t, err)
				require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "self-id", "manual"))
			} else {
				owner = f.EnsureParticipant("owner@example.test", "Owner", "example.test")
				if !tc.selfEvidence {
					require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
				}
			}

			departed := f.EnsureParticipant("departed@example.test", "Departed", "example.test")
			require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, departed, "member"))
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
			if tc.selfEvidence {
				writeDeliveryChatEvidence(t, f, owner)
			} else {
				writeDeliveryChatEvidence(t, f)
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
			err = f.Store.WithDeliveryAdmissionContext(t.Context(), recipients, func(context.Context) error { calls++; return nil })
			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, 1, calls)
			} else {
				var denied *store.DeliveryAdmissionError
				require.ErrorAs(t, err, &denied)
				assert.Equal(t, "draft_required", denied.Code)
				assert.Equal(t, tc.reason, denied.Reason)
				assert.Zero(t, calls)
			}
		})
	}
}

func TestChatDeliveryPolicyReadValidation(t *testing.T) {
	for _, name := range []string{"endpoint", "unsupported kind", "unknown route", "owner-only roster"} {
		t.Run(name, func(t *testing.T) {
			f := deliveryChatFixture(t)
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			p, _, err := f.Store.CreatePersonFromParticipant(peer)
			require.NoError(t, err)
			q := store.DeliveryPolicyQuery{PersonUID: p.VCardUID, RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread"}
			if name != "unknown route" {
				require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
				if name == "owner-only roster" {
					require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "peer@example.test", "manual"))
				}
				writeDeliveryChatEvidence(t, f)
			}
			switch name {
			case "endpoint":
				q.Endpoint = "peer@example.test"
			case "unsupported kind":
				q.RouteKind = "unsupported"
			case "unknown route":
				other, err := f.Store.GetOrCreateSource("beeper", "other-account")
				require.NoError(t, err)
				q.SourceID = other.ID
			case "owner-only roster":
				recipients, err := f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "chat-thread")
				require.NoError(t, err)
				require.Empty(t, recipients)
			}
			_, err = f.Store.GetDeliveryPolicyContext(t.Context(), q)
			require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
		})
	}
}

func writeDeliveryChatEvidence(t *testing.T, f *storetest.Fixture, self ...int64) store.MessagingRouteEvidence {
	t.Helper()
	rows, err := f.Store.DB().QueryContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM conversation_participants WHERE conversation_id=? AND left_at IS NULL ORDER BY participant_id`), f.ConvID)
	require.NoError(t, err)
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	var kind string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT conversation_type FROM conversations WHERE id=?`), f.ConvID).Scan(&kind))
	providerType := "single"
	if kind == "group_chat" {
		providerType = "group"
	}
	e := store.MessagingRouteEvidence{ChatID: "chat-thread", AccountID: "synthetic-account", Network: "whatsapp", ProviderType: providerType, ObservedAt: time.Now(), MembershipComplete: true, ParticipantIDs: ids, SelfParticipantIDs: self}
	require.NoError(t, f.Store.SetConversationMessagingRouteEvidence(t.Context(), f.ConvID, e))
	return e
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

func TestChatRouteAdmissionInvalidProof(t *testing.T) {
	t.Run("missing conversation", func(t *testing.T) {
		f := storetest.New(t)
		_, err := f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "")
		require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
	})
	for _, name := range []string{"absent", "account mismatch", "roster mismatch", "failed", "merged", "account failure", "reanchor required"} {
		t.Run(name, func(t *testing.T) {
			f := deliveryChatFixture(t)
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
			if name != "absent" {
				e := writeDeliveryChatEvidence(t, f)
				switch name {
				case "account failure", "reanchor required":
					testDeliverySourceProofRevocation(t, f, peer, name)
					return
				case "account mismatch":
					e.AccountID = "other"
				case "roster mismatch":
					e.ParticipantIDs = nil
				case "failed":
					e.Failure = "membership_fetch_failed"
				case "merged":
					e.Merged = true
				}
				require.NoError(t, f.Store.SetConversationMessagingRouteEvidence(t.Context(), f.ConvID, e))
			}
			_, err := f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "chat-thread")
			require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
		})
	}
}

func testDeliverySourceProofRevocation(t *testing.T, f *storetest.Fixture, peer int64, signal string) {
	t.Helper()
	p, _, err := f.Store.CreatePersonFromParticipant(peer)
	require.NoError(t, err)
	setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
	q := []store.DeliveryPolicyQuery{{RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread"}}
	for i, revoked := range []bool{false, true, false} {
		if i > 0 {
			if signal == "account failure" {
				failure := ""
				if revoked {
					failure = "account_binding_mismatch"
				}
				require.NoError(t, f.Store.SetSourceMessagingRouteFailureContext(t.Context(), f.Source.ID, failure))
			} else if revoked {
				require.NoError(t, f.Store.SetArchiveMarker(t.Context(), store.BeeperReanchorMarkerKey(f.Source.ID), "synthetic anchor mismatch"))
			} else {
				_, err := f.Store.UpdateSourceSettingsContext(t.Context(), f.Source.ID, store.SourceSettingsUpdate{AcceptReanchor: true})
				require.NoError(t, err)
			}
		}
		calls := 0
		err := f.Store.WithDeliveryAdmissionContext(t.Context(), q, func(context.Context) error { calls++; return nil })
		if revoked {
			_, routeErr := f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "chat-thread")
			require.ErrorIs(t, routeErr, store.ErrDeliveryPolicyInvalid)
			require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
			assert.Zero(t, calls)
		} else {
			require.NoError(t, err)
			assert.Equal(t, 1, calls)
		}
	}
}
