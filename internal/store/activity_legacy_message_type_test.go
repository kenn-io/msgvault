package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLegacyNullableMessageTypeRemainsVisibleInActivity(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
	requirements.NoError(st.AddAccountIdentity(sourceID, storetest.OwnerAddress, "manual"))
	peerID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	requirements.NoError(err)
	occurredAt := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	messageID := persistLegacyLoopEmail(t, st, sourceID, conversationID, "legacy-null-activity",
		"alice@example.test", storetest.OwnerAddress, "Synthetic message", occurredAt)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE messages SET message_type = NULL WHERE id = ?`), messageID)
	requirements.NoError(err)

	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 100})
	requirements.NoError(err)
	_, err = projector.RunBackstop(t.Context())
	requirements.NoError(err)

	filters := store.ActivityFilters{IncludeNoise: true}
	messages, err := st.ListActivityMessagesContext(t.Context(), store.MessageQuery{
		ConversationID: conversationID,
		Limit:          10,
		Filters:        filters,
	})
	requirements.NoError(err)
	requirements.Len(messages, 1)
	assertions.Equal(messageID, messages[0].MessageID)

	latest, err := st.LatestEligibleMessageContext(t.Context(), conversationID)
	requirements.NoError(err)
	assertions.Equal(messageID, latest)
	display, err := st.MessageDisplayContext(t.Context(), []int64{messageID})
	requirements.NoError(err)
	requirements.Contains(display, messageID)
	assertions.True(store.IsEmailMessageType(display[messageID].MessageType))

	query := store.BurstQuery{
		FromDate: "2026-10-01", ToDate: "2026-10-31", Limit: 10,
		Filters: filters,
	}
	warm, err := st.ListActivityBurstsContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(warm.Bursts, 1)
	assertions.Equal(1, warm.Bursts[0].Inbound)

	summary, err := st.MessageContactAggregateContext(t.Context(), store.ContactQuery{
		Subject:  store.ActivitySubject{ParticipantIDs: []int64{peerID}},
		Now:      occurredAt,
		Location: time.UTC,
	})
	requirements.NoError(err)
	assertions.Equal(1, summary.Count)

	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), sourceID, "legacy-null-thread", conversationpolicy.Cold))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE conversation_policy SET effective_tier = ? WHERE conversation_id = ?`),
		string(conversationpolicy.Cold), conversationID)
	requirements.NoError(err)
	cold, err := st.ListColdActivityBurstsContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(cold.Bursts, 1)
	assertions.Equal(1, cold.Bursts[0].Inbound)
	coldCount, err := st.CountColdActivityMessagesContext(t.Context(), query)
	requirements.NoError(err)
	assertions.Equal(1, coldCount)
}
