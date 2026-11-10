package store_test

import (
	"context"
	"database/sql"
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
		{name: "approved", approve: true},
		{name: "scoped incoming only", approve: true, scopedOwner: true},
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
			owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			original := peer
			if tc.lateBinding {
				original = f.EnsureParticipant("original@example.test", "Original", "example.test")
			}
			p, _, err := f.Store.CreatePersonFromParticipant(original)
			require.NoError(t, err)
			if !tc.selfEvidence {
				require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
			}
			if tc.scopedOwner {
				owner, err = f.Store.EnsureParticipantByIdentifier("beeper", beeperidentity.Fallback("synthetic-account", "self-id"), "Owner")
				require.NoError(t, err)
				require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "self-id", "manual"))
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

func TestChatRouteAdmissionQueryValidation(t *testing.T) {
	f := deliveryChatFixture(t)
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	p, _, err := f.Store.CreatePersonFromParticipant(peer)
	require.NoError(t, err)
	require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
	writeDeliveryChatEvidence(t, f)
	for _, kind := range []string{"endpoint", "unsupported kind"} {
		t.Run(kind, func(t *testing.T) {
			q := store.DeliveryPolicyQuery{PersonUID: p.VCardUID, RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread"}
			if kind == "endpoint" {
				q.Endpoint = "peer@example.test"
			} else {
				q.RouteKind = "unsupported"
			}
			_, err := f.Store.GetDeliveryPolicyContext(t.Context(), q)
			require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
		})
	}
	_, err = f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "")
	require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
}

func TestChatDeliveryPolicyReadRequiresCurrentMember(t *testing.T) {
	for _, name := range []string{"wrong source", "owner-only roster"} {
		t.Run(name, func(t *testing.T) {
			f := deliveryChatFixture(t)
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			p, _, err := f.Store.CreatePersonFromParticipant(peer)
			require.NoError(t, err)
			require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
			setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
			q := store.DeliveryPolicyQuery{PersonUID: p.VCardUID, RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread"}
			if name == "wrong source" {
				other, err := f.Store.GetOrCreateSource("beeper", "other-account")
				require.NoError(t, err)
				q.SourceID = other.ID
			} else {
				owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
				require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
				require.NoError(t, f.Store.ReplaceConversationParticipants(f.ConvID, []store.ConversationParticipantRef{{ParticipantID: owner}}))
			}
			writeDeliveryChatEvidence(t, f)
			if name == "owner-only roster" {
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
	for _, name := range []string{"absent", "account mismatch", "roster mismatch", "failed", "merged", "interrupted refresh"} {
		t.Run(name, func(t *testing.T) {
			f := deliveryChatFixture(t)
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			p, _, err := f.Store.CreatePersonFromParticipant(peer)
			require.NoError(t, err)
			require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
			e := writeDeliveryChatEvidence(t, f)
			setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
			switch name {
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
			switch name {
			case "absent":
				require.NoError(t, f.Store.SetConversationMetadata(f.ConvID, sql.NullString{}))
			case "interrupted refresh":
				require.NoError(t, f.Store.InvalidateConversationMessagingRouteEvidence(t.Context(), f.ConvID))
			}
			_, err = f.Store.ChatRouteRecipientsContext(t.Context(), f.Source.ID, "chat-thread")
			require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
		})
	}
}

func TestChatRouteAdmissionSourceProofRevocation(t *testing.T) {
	for _, signal := range []string{"account failure", "reanchor required"} {
		t.Run(signal, func(t *testing.T) {
			f := deliveryChatFixture(t)
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			p, _, err := f.Store.CreatePersonFromParticipant(peer)
			require.NoError(t, err)
			require.NoError(t, f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, peer, "member"))
			writeDeliveryChatEvidence(t, f)
			setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
			for _, revoked := range []bool{false, true, false} {
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
				calls := 0
				err := f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryPolicyQuery{{RouteKind: "chat", SourceID: f.Source.ID, SourceConversationID: "chat-thread"}}, func(context.Context) error { calls++; return nil })
				if revoked {
					var denied *store.DeliveryAdmissionError
					require.ErrorAs(t, err, &denied)
					assert.Equal(t, "draft_required", denied.Code)
					assert.Equal(t, "unresolved_recipient", denied.Reason)
					assert.Zero(t, calls)
				} else {
					require.NoError(t, err)
					assert.Equal(t, 1, calls)
				}
			}
		})
	}
}
