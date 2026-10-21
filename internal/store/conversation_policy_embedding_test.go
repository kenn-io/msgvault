package store_test

import (
	"database/sql"
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

func TestConversationPolicyChangeInvalidatesSharedParticipantContextEmbeddings(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "contextual-embedding-policy")
	requirements.NoError(err)
	changedRoom, err := s.EnsureConversation(source.ID, "changed", "Synthetic changed room")
	requirements.NoError(err)
	targetRoom, err := s.EnsureConversation(source.ID, "target", "Synthetic target room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "changed", conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "target", conversationpolicy.Full))

	identity := store.ConversationParticipantIdentity{
		IdentifierType: "beeper", IdentifierValue: "synthetic:shared-member", DisplayName: "Synthetic Shared Member",
	}
	shared, err := s.ResolveConversationParticipantContext(t.Context(), changedRoom, identity)
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(t.Context(), changedRoom, shared.ParticipantID, "member"))
	targetMember, err := s.ResolveConversationParticipantContext(t.Context(), targetRoom, identity)
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(t.Context(), targetRoom, targetMember.ParticipantID, "member"))
	assertions.Equal(shared.ParticipantID, targetMember.ParticipantID)

	messageID, err := s.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: targetRoom,
		SourceMessageID: "target-context-message", MessageType: "beeper",
	})
	requirements.NoError(err)
	requirements.NoError(s.SetEmbedGen(t.Context(), []int64{messageID}, 37))
	var before sql.NullInt64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), messageID).Scan(&before))
	assertions.True(before.Valid)
	assertions.Equal(int64(37), before.Int64)
	var beforeRevision int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&beforeRevision))

	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "changed", conversationpolicy.Cold))
	var after sql.NullInt64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), messageID).Scan(&after))
	assertions.False(after.Valid, "a Full message with changed roster context must be re-embedded")
	var afterRevision int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&afterRevision))
	assertions.Greater(afterRevision, beforeRevision, "a stale contextual vector must fail the publication revision check")
	ids, err := s.ScanForEmbedding(t.Context(), 37, 0, 10)
	requirements.NoError(err)
	assertions.Contains(ids, messageID, "the invalidated message must be discoverable by the embed worker")
}

func TestConversationPolicyChangeInvalidatesDepartedSenderContextEmbeddings(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "contextual-embedding-policy")
	requirements.NoError(err)
	changedRoom, err := s.EnsureConversation(source.ID, "changed", "Synthetic changed room")
	requirements.NoError(err)
	targetRoom, err := s.EnsureConversation(source.ID, "target", "Synthetic target room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "changed", conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "target", conversationpolicy.Full))

	identity := store.ConversationParticipantIdentity{
		IdentifierType: "beeper", IdentifierValue: "synthetic:shared-member", DisplayName: "Synthetic Shared Member",
	}
	shared, err := s.ResolveConversationParticipantContext(t.Context(), changedRoom, identity)
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(t.Context(), changedRoom, shared.ParticipantID, "member"))
	targetMember, err := s.ResolveConversationParticipantContext(t.Context(), targetRoom, identity)
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(t.Context(), targetRoom, targetMember.ParticipantID, "member"))
	assertions.Equal(shared.ParticipantID, targetMember.ParticipantID)

	messageID, err := s.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: targetRoom,
		SourceMessageID: "target-context-message", MessageType: "beeper",
		SenderID: sql.NullInt64{Int64: shared.ParticipantID, Valid: true},
	})
	requirements.NoError(err)
	// The latest block still contains this sender after they leave the roster.
	requirements.NoError(s.ReplaceConversationParticipantsContext(t.Context(), targetRoom, nil))
	requirements.NoError(s.SetEmbedGen(t.Context(), []int64{messageID}, 37))
	var before sql.NullInt64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), messageID).Scan(&before))
	assertions.True(before.Valid)
	assertions.Equal(int64(37), before.Int64)
	var beforeRevision int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&beforeRevision))

	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "changed", conversationpolicy.Cold))
	var after sql.NullInt64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), messageID).Scan(&after))
	assertions.False(after.Valid, "a Full message with changed roster context must be re-embedded")
	var afterRevision int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&afterRevision))
	assertions.Greater(afterRevision, beforeRevision, "a stale contextual vector must fail the publication revision check")
	ids, err := s.ScanForEmbedding(t.Context(), 37, 0, 10)
	requirements.NoError(err)
	assertions.Contains(ids, messageID, "the invalidated message must be discoverable by the embed worker")
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
