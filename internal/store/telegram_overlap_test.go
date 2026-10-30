package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTelegramBeeperOverlapRequiresSelfEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-bridge")
	require.NoError(err)
	person, err := st.EnsureParticipantByIdentifier("beeper", "synthetic-user", "Synthetic")
	require.NoError(err)
	var conversation int64
	require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO conversations(source_id, source_conversation_id, conversation_type) VALUES (?, ?, ?) RETURNING id`), source.ID, "synthetic-chat", "direct_chat").Scan(&conversation))
	require.NoError(st.EnsureConversationParticipantContext(ctx, conversation, person, "member"))
	key := beeperidentity.TelegramStableProviderKey("999")
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO participant_contact_observations(participant_id,source_id,address_kind,provider_user_id,original_value,normalized_value,source) VALUES (?,?,?,?,?,?,?)`), person, source.ID, "provider_user_id", key, "synthetic", "synthetic", "beeper")
	require.NoError(err)
	evidence := store.MessagingRouteEvidence{AccountID: source.Identifier, Network: "Telegram", ChatID: "synthetic-chat", ObservedAt: time.Unix(100, 0), ParticipantIDs: []int64{person}}
	require.NoError(st.SetConversationMessagingRouteEvidence(ctx, conversation, evidence))
	found, err := st.HasBeeperTelegramOwnerContext(ctx, 999)
	require.NoError(err)
	assert.False(found, "a contact is not proof of an overlapping owner")
	evidence.SelfParticipantIDs = []int64{person}
	require.NoError(st.SetConversationMessagingRouteEvidence(ctx, conversation, evidence))
	found, err = st.HasBeeperTelegramOwnerContext(ctx, 999)
	require.NoError(err)
	assert.True(found)
	found, err = st.HasBeeperTelegramOwnerContext(ctx, 998)
	require.NoError(err)
	assert.False(found)
}
