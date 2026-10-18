package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicyEmbeddingScanCoverageAndHydration(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "embedding-policy")
	requirements.NoError(err)
	room, err := s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	id, err := s.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper"})
	requirements.NoError(err)
	ids, err := s.ScanForEmbedding(t.Context(), 1, 0, 100)
	requirements.NoError(err)
	assertions.Empty(ids)
	live, stamped, _, _, err := s.CoverageCounts(t.Context(), 1)
	requirements.NoError(err)
	assertions.Zero(live)
	assertions.Zero(stamped)
	requirements.NoError(s.SetEmbedGen(t.Context(), []int64{id}, 1))
	var stamp any
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), id).Scan(&stamp))
	assertions.Nil(stamp, "cold messages cannot be stamped covered")
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	ids, err = s.ScanForEmbedding(t.Context(), 1, 0, 100)
	requirements.NoError(err)
	assertions.Equal([]int64{id}, ids)
}

func TestConversationPolicyPromotionBackfillsBelowWatermark(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "promotion-policy")
	requirements.NoError(err)
	room, err := s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	id, err := s.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "old", MessageType: "beeper"})
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	ctx, err := s.CaptureConversationPolicyWork(t.Context())
	requirements.NoError(err)
	requirements.NoError(s.SetEmbedGen(ctx, []int64{id}, 1))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Cold))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	requirements.ErrorIs(s.SetEmbedGen(ctx, []int64{id}, 1), store.ErrConversationPolicyStale)
	ids, err := s.ScanForEmbedding(t.Context(), 1, 0, 10)
	requirements.NoError(err)
	assertions.Equal([]int64{id}, ids)
}
