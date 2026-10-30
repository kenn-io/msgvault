package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A completed write may be interrupted before its provider batching seam.
// Configure/expiry must recover that hint without requiring another sync.
func TestConversationPolicyActivityDirtyRecovery(t *testing.T) {
	for _, operation := range []string{"upsert", "persist", "mention", "reply", "clear reply", "replace reply", "withdraw own", "move own", "from"} {
		t.Run(operation, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newActivityFixture(t)
			settings := conversationpolicy.RecommendedSettings()
			parent := f.message(t, "parent", activityNow.AddDate(-3, 0, 0), true)
			child := f.message(t, "child", activityNow.Add(-time.Hour), false)
			if operation == "clear reply" || operation == "replace reply" {
				requirements.NoError(f.s.SetMessageReplyContext(t.Context(), child, parent))
			}
			// Create the replacement target before activity hints are drained,
			// so only the reply replacement itself can mark the room dirty.
			var peer int64
			if operation == "replace reply" {
				peer = f.message(t, "peer", activityNow.AddDate(-2, 0, 0), false)
			}
			if operation == "withdraw own" || operation == "move own" {
				f.message(t, "child", activityNow.Add(-time.Hour), true)
			}
			_, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
			requirements.NoError(err)
			var want = conversationpolicy.Full
			switch operation {
			case "upsert":
				f.message(t, "child", activityNow.Add(-time.Hour), true)
			case "persist":
				_, err = f.s.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: f.source, ConversationID: f.room, SourceMessageID: "persisted", MessageType: "slack", SentAt: sql.NullTime{Time: activityNow.Add(-time.Hour), Valid: true}, IsFromMe: true}})
				requirements.NoError(err)
			case "mention", "from":
				requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), child, operation, []int64{f.self}, nil))
			case "reply":
				requirements.NoError(f.s.SetReplyToContext(t.Context(), f.source, "child", "parent"))
			case "clear reply":
				requirements.NoError(f.s.ClearMessageRepliesContext(t.Context(), f.source, []int64{child}))
				want = conversationpolicy.Cold
			case "replace reply":
				requirements.NoError(f.s.SetMessageReplyContext(t.Context(), child, peer))
				want = conversationpolicy.Cold
			case "withdraw own":
				f.message(t, "child", activityNow.Add(-time.Hour), false)
				want = conversationpolicy.Cold
			case "move own":
				room, err := f.s.EnsureConversation(f.source, "moved", "Moved")
				requirements.NoError(err)
				_, err = f.s.UpsertMessage(&store.Message{SourceID: f.source, ConversationID: room, SourceMessageID: "child", MessageType: "slack", SentAt: sql.NullTime{Time: activityNow.Add(-time.Hour), Valid: true}, IsFromMe: true})
				requirements.NoError(err)
				want = conversationpolicy.Cold
			}
			// Empty explicit refresh never drains dirty state or changes eligibility.
			before := f.tier(t)
			_, err = f.s.RefreshConversationPolicy(t.Context(), []int64{}, activityNow)
			requirements.NoError(err)
			assertions.Equal(before, f.tier(t))
			_, err = f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
			requirements.NoError(err)
			assertions.Equal(want, f.tier(t))
		})
	}
}

func TestConversationPolicyActivityBeeperSelfProof(t *testing.T) {
	for _, variant := range []string{"unknown roster", "wrong account", "wrong room", "network label", "bad network", "account failure", "remove proof", "membership fetch failure", "deleted source messages", "room account lookup failure", "room not connected", "room account binding failure", "room chat binding failure", "room network failure", "unknown proof failure"} {
		t.Run(variant, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s := testutil.NewTestStore(t)
			src, err := s.GetOrCreateSource("beeper", "account")
			requirements.NoError(err)
			room, err := s.EnsureConversationWithType(src.ID, "room", "group_chat", "Room")
			requirements.NoError(err)
			self, err := s.EnsureParticipant("member@example.test", "Untrusted name", "example.test")
			requirements.NoError(err)
			id, err := s.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "mention", MessageType: "beeper", SentAt: sql.NullTime{Time: activityNow.Add(-time.Hour), Valid: true}})
			requirements.NoError(err)
			requirements.NoError(s.ReplaceMessageRecipientsContext(t.Context(), id, "mention", []int64{self}, nil))
			_, err = s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), activityNow)
			requirements.NoError(err)
			proof := store.MessagingRouteEvidence{ChatID: "room", AccountID: "account", Network: "slack", ProviderType: "group", ObservedAt: activityNow.Add(-time.Hour), SelfParticipantIDs: []int64{self}}
			switch variant {
			case "wrong account":
				proof.AccountID = "other"
			case "wrong room":
				proof.ChatID = "other"
			case "network label":
				proof.Network = ""
				proof.NetworkLabel = "Slack"
			case "bad network":
				proof.Network = "made up"
			case "membership fetch failure":
				proof.Failure = "membership_fetch_failed"
			case "deleted source messages":
				proof.Failure = "source_messages_deleted"
			case "room account lookup failure":
				proof.Failure = "account_lookup_failed"
			case "room not connected":
				proof.Failure = "account_not_connected"
			case "room account binding failure":
				proof.Failure = "account_binding_mismatch"
			case "room chat binding failure":
				proof.Failure = "chat_binding_mismatch"
			case "room network failure":
				proof.Failure = "network_unverified"
			case "unknown proof failure":
				proof.Failure = "unknown"
			}
			requirements.NoError(s.SetConversationMessagingRouteEvidence(t.Context(), room, proof))
			if variant == "account failure" {
				requirements.NoError(s.SetSourceMessagingRouteFailureContext(t.Context(), src.ID, "account_binding_mismatch"))
			}
			_, err = s.ExpireConversationPolicy(t.Context(), activityNow)
			requirements.NoError(err)
			rows, err := s.ListConversationPolicies(t.Context())
			requirements.NoError(err)
			requirements.Len(rows, 1)
			want := conversationpolicy.Cold
			if variant == "unknown roster" || variant == "remove proof" || variant == "membership fetch failure" || variant == "deleted source messages" {
				want = conversationpolicy.Full
			}
			assertions.Equal(want, rows[0].EffectiveTier)
			if variant == "remove proof" {
				proof.SelfParticipantIDs = nil
				requirements.NoError(s.SetConversationMessagingRouteEvidence(t.Context(), room, proof))
				_, err = s.ExpireConversationPolicy(t.Context(), activityNow)
				requirements.NoError(err)
				rows, err = s.ListConversationPolicies(t.Context())
				requirements.NoError(err)
				assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
			}
		})
	}
}

func TestConversationPolicyActivityUsesSelfProofWithTruncatedRoster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "large-room-account")
	requirements.NoError(err)
	room, err := s.EnsureConversationWithType(source.ID, "large-room", "group_chat", "Large room")
	requirements.NoError(err)
	participants := make([]int64, 101)
	for index := range participants {
		address := fmt.Sprintf("member-%03d@example.test", index)
		name := fmt.Sprintf("Member %03d", index)
		participants[index], err = s.EnsureParticipant(address, name, "example.test")
		requirements.NoError(err)
		requirements.NoError(s.EnsureConversationParticipant(room, participants[index], "member"))
	}
	mentionedAt := activityNow.Add(-time.Hour)
	messageID, err := s.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: room, SourceMessageID: "recent-self-mention",
		MessageType: "beeper", SentAt: sql.NullTime{Time: mentionedAt, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(s.ReplaceMessageRecipientsContext(t.Context(), messageID, "mention", []int64{participants[0]}, []string{"Member 000"}))
	_, err = s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), activityNow)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationMessagingRouteEvidence(t.Context(), room, store.MessagingRouteEvidence{
		ChatID: "large-room", AccountID: "large-room-account", Network: "whatsapp", ProviderType: "group",
		ObservedAt: activityNow.Add(-time.Minute), MembershipComplete: true,
		ParticipantIDs: participants, SelfParticipantIDs: []int64{participants[0]},
	}))
	_, err = s.ExpireConversationPolicy(t.Context(), activityNow)
	requirements.NoError(err)
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier,
		"a bounded self-identity proof survives truncation of the general room roster")
}

func TestConversationPolicyActivityDirtyFailureIsRetryable(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	_, err := f.s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), activityNow)
	requirements.NoError(err)
	f.message(t, "own", activityNow.Add(-time.Hour), true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.s.ExpireConversationPolicy(ctx, activityNow)
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
	if f.s.IsPostgreSQL() {
		_, err = f.s.DB().Exec(`ALTER TABLE conversation_policy ADD CONSTRAINT reject_activity_promotion CHECK(effective_tier<>'full')`)
	} else {
		_, err = f.s.DB().Exec(`CREATE TRIGGER reject_activity_promotion BEFORE UPDATE ON conversation_policy WHEN NEW.effective_tier='full' BEGIN SELECT RAISE(ABORT,'injected activity promotion failure'); END`)
	}
	requirements.NoError(err)
	_, err = f.s.ExpireConversationPolicy(t.Context(), activityNow)
	requirements.Error(err)
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
	if f.s.IsPostgreSQL() {
		_, err = f.s.DB().Exec(`ALTER TABLE conversation_policy DROP CONSTRAINT reject_activity_promotion`)
	} else {
		_, err = f.s.DB().Exec(`DROP TRIGGER reject_activity_promotion`)
	}
	requirements.NoError(err)
	_, err = f.s.ExpireConversationPolicy(t.Context(), activityNow)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, f.tier(t))
}

func TestConversationPolicyActivityUnchangedSourceProofDoesNotDirtyHistory(t *testing.T) {
	for _, failure := range []string{"", "account_not_connected"} {
		t.Run(failure, func(t *testing.T) {
			requirements := require.New(t)

			f := newActivityFixture(t)
			f.message(t, "own", activityNow, true)
			requirements.NoError(f.s.SetSourceMessagingRouteFailureContext(t.Context(), f.source, failure))
			settings := conversationpolicy.RecommendedSettings()
			_, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
			requirements.NoError(err)
			requirements.NoError(f.s.SetSourceMessagingRouteFailureContext(t.Context(), f.source, failure))
			_, err = f.s.DB().Exec(`ALTER TABLE messages RENAME TO activity_hidden_messages`)
			requirements.NoError(err)
			_, err = f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
			requirements.NoError(err)
		})
	}
}

func TestConversationPolicyActivityPersistNewRoomRecoversAfterInterruptedBatch(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	_, err := f.s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), activityNow)
	requirements.NoError(err)
	id, err := f.s.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: f.source, SourceMessageID: "inline-own", MessageType: "slack", IsFromMe: true, SentAt: sql.NullTime{Time: activityNow.Add(-time.Hour), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "inline-room", ConversationType: "group_chat", Title: "Inline room"},
	})
	requirements.NoError(err)
	requirements.NoError(f.s.DB().QueryRow(f.s.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), id).Scan(&f.room))
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
	_, err = f.s.ExpireConversationPolicy(t.Context(), activityNow)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, f.tier(t))
}
