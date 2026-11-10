package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestConversationParticipantBatchesApplyRolesAndRemovals(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newEmbeddingJournalFixture(t)
	messageID := f.insertMessage(t, "roster-message", "beeper", f.oldConversationID, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "Synthetic body")
	before, err := f.store.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(err)
	require.NoError(f.store.ApplyConversationParticipantBatchContext(t.Context(), f.oldConversationID, []store.ConversationParticipantRef{{ParticipantID: f.aliceID, Role: "member"}, {ParticipantID: f.bobID, Role: "member"}}, nil))
	require.NoError(f.store.ApplyConversationParticipantBatchContext(t.Context(), f.oldConversationID, []store.ConversationParticipantRef{{ParticipantID: f.aliceID, Role: "admin"}}, []int64{f.bobID}))
	members, err := f.store.ConversationParticipantsContext(t.Context(), f.oldConversationID)
	require.NoError(err)
	assert.Equal([]store.ConversationParticipantRef{{ParticipantID: f.aliceID, Role: "admin"}}, members)
	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 2, "each batch emits one coalesced membership change")
	assert.Equal(store.EmbeddingChangeConversationParticipant, changes[0].Kind)
	assert.Equal(store.EmbeddingChangeConversationParticipant, changes[1].Kind)
	oversized := make([]store.ConversationParticipantRef, 17)
	require.Error(f.store.ApplyConversationParticipantBatchContext(t.Context(), f.oldConversationID, oversized, nil))
	other, err := f.store.GetOrCreateSource("beeper", "other-synthetic-account")
	require.NoError(err)
	syncID, err := f.store.StartSync(other.ID, "full")
	require.NoError(err)
	scoped := f.store.ScopedToSync(other.ID, syncID)
	require.Error(scoped.ApplyConversationParticipantBatchContext(t.Context(), f.oldConversationID, nil, []int64{f.aliceID}))
	members, err = f.store.ConversationParticipantsContext(t.Context(), f.oldConversationID)
	require.NoError(err)
	assert.Len(members, 1)
}
