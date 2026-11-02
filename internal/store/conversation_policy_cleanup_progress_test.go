package store_test

import (
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

func TestDocumentVisualColdPolicyCleanupRepairsProgressAndUnpreparedClaims(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f, doc := seedDocumentVectorGenerationWithChunks(t, 1)
	ctx := t.Context()
	f.Store.DB().SetMaxOpenConns(1)
	visual := createVisualGeneration(t, f)
	owner, _, fence := createVisualOwner(t, f, "unprepared-owner")
	_ = claimVisualOwner(t, f, visual.ID, owner, "pending", fence)
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(ctx, "visual/"+visual.Fingerprint)
	requirements.NoError(err)
	requirements.NoError(f.Store.CompleteAttachmentChangeReconciliation(ctx, consumer.ConsumerKey, consumer.BaselineSequence))
	requirements.NoError(f.Store.AdvanceVisualGenerationSourceFence(ctx, visual.ID, fence))
	requirements.NoError(f.Store.CheckpointDocumentVectorBuildForFingerprint(ctx, doc.ID, doc.Fingerprint, 100, false, store.DocumentVectorUsageDelta{}, time.Now().UTC()))
	// A later attachment change moves the journal past the consumer baseline.
	later := f.CreateMessage("later-attachment")
	requirements.NoError(f.Store.UpsertAttachmentRecord(ctx, later, store.AttachmentWrite{ContentHash: fmt.Sprintf("%064x", 77), Filename: "later.png", StoragePath: "later", State: "stored"}))
	var journalEnd int64
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM attachment_change_log`).Scan(&journalEnd))
	requirements.Greater(journalEnd, consumer.BaselineSequence)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	requirements.NoError(f.Store.ReconcileConversationPolicyMediaProgress(ctx))
	cursor, err := f.Store.GetDocumentVectorBuildCursor(ctx, doc.ID)
	requirements.NoError(err)
	assertions.Zero(cursor)
	current, err := f.Store.GetVisualGeneration(ctx, visual.ID)
	requirements.NoError(err)
	assertions.Zero(current.SourceFence)
	consumer, _, err = f.Store.RegisterAttachmentChangeConsumer(ctx, consumer.ConsumerKey)
	requirements.NoError(err)
	assertions.False(consumer.ReconciliationComplete)
	assertions.GreaterOrEqual(consumer.BaselineSequence, journalEnd, "the reopened scan starts at the current journal boundary")
	assertions.Equal(consumer.BaselineSequence, consumer.LastSequence)
	var claims int
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_work_claims`).Scan(&claims))
	assertions.Zero(claims, "claim-only work must not survive exclusion")
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	_ = claimVisualOwner(t, f, visual.ID, owner, "fresh", fence)
	candidates, err := f.Store.ListDocumentVectorChunkCandidates(ctx, doc.ID, cursor, 10)
	requirements.NoError(err)
	assertions.Len(candidates, 1, "ordinary scan rediscovers low chunk after promotion")
}

func TestDocumentVisualColdPolicyCleanupPreservesCurrentClaimAcrossBatches(t *testing.T) {
	requirements := require.New(t)

	f, _ := seedDocumentVectorGenerationWithChunks(t, 1)
	ctx := t.Context()
	f.Store.DB().SetMaxOpenConns(1)
	generation := createVisualGeneration(t, f)
	owner, _, fence := createVisualOwner(t, f, "many-old-claims")
	current := claimVisualOwner(t, f, generation.ID, owner, "current", fence)
	// Persisted retry revisions share one owner; only their source stamps are stale.
	tx, err := f.Store.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	for i := range 501 {
		_, err = tx.ExecContext(ctx, f.Store.Rebind(`INSERT INTO visual_work_claims(generation_id,message_id,blob_hash,media_input_key,proposed_revision,lease_owner,lease_expires_at,fencing_token,source_fence,claimed_content_stamp) SELECT generation_id,message_id,blob_hash,media_input_key,?,lease_owner,lease_expires_at,fencing_token,source_fence,'obsolete-stamp' FROM visual_work_claims WHERE proposed_revision='current'`), fmt.Sprintf("stale-%03d", i))
		requirements.NoError(err)
	}
	requirements.NoError(tx.Commit())
	requirements.NoError(f.Store.ReconcileConversationPolicyMediaProgress(ctx))
	var count int
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_work_claims`).Scan(&count))
	assert.Equal(t, 1, count, "all 501 stale rows disappear, current same-owner revision survives")
	requirements.NoError(f.Store.RenewVisualWork(ctx, current, time.Now().UTC(), time.Minute))
}

// An archive that classified rooms before derived cleanup existed has cold
// rooms but no cleanup target. Opening it must leave cleanup pending.
func TestConversationPolicyCleanupSeededForArchiveWithColdRooms(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	ctx := t.Context()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic:upgrade")
	requirements.NoError(err)
	_, err = st.EnsureConversation(source.ID, "room", "Room")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "room", conversationpolicy.Cold))
	_, err = st.DB().ExecContext(ctx, `DELETE FROM archive_metadata WHERE key IN ('conversation_policy_revision','conversation_policy_cleanup_revision','conversation_policy_cleaned_revision')`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "conversation_policy_cleanup_seed_v1")
	requirements.NoError(err)
	_, pending, err := st.ConversationPolicyCleanupPending(ctx)
	requirements.NoError(err)
	requirements.False(pending, "the simulated upgrade starts without a cleanup target")

	requirements.NoError(st.InitSchema())
	target, pending, err := st.ConversationPolicyCleanupPending(ctx)
	requirements.NoError(err)
	assertions.True(pending)
	acknowledged, err := st.AcknowledgeConversationPolicyCleanup(ctx, target)
	requirements.NoError(err)
	requirements.True(acknowledged)

	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "room", conversationpolicy.Full))
	_, pending, err = st.ConversationPolicyCleanupPending(ctx)
	requirements.NoError(err)
	assertions.True(pending, "a later transition moves the target past the acknowledged seed")
}

// A message that leaves a full room for a cold one changes no room tier, yet
// its text lives in the vectors of its old window. Cleanup must start anyway.
func TestConversationPolicyCleanupScheduledWhenMessageLeavesFullRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	ctx := t.Context()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic:move")
	requirements.NoError(err)
	rooms := map[string]int64{}
	for name, tier := range map[string]conversationpolicy.Tier{"full": conversationpolicy.Full, "other-full": conversationpolicy.Full, "cold": conversationpolicy.Cold} {
		rooms[name], err = st.EnsureConversation(source.ID, name, name)
		requirements.NoError(err)
		requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, name, tier))
	}
	move := func(room string) bool {
		t.Helper()
		_, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: rooms[room], SourceMessageID: "moved", MessageType: "slack"})
		requirements.NoError(err)
		target, pending, err := st.ConversationPolicyCleanupPending(ctx)
		requirements.NoError(err)
		if pending {
			acknowledged, err := st.AcknowledgeConversationPolicyCleanup(ctx, target)
			requirements.NoError(err)
			requirements.True(acknowledged)
		}
		return pending
	}
	move("full")
	assertions.False(move("other-full"), "a move between full rooms changes no eligibility")
	assertions.True(move("cold"), "leaving a full room for a cold one needs cleanup")
	assertions.False(move("cold"), "an unchanged room is not a move")
}

// Search accepts a vector while its message revision matches. A moved
// message's former window siblings must stop matching before cleanup runs.
func TestConversationPolicyMoveRevokesOldWindowContext(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	ctx := t.Context()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic:move-context")
	requirements.NoError(err)
	rooms := map[string]int64{}
	for name, tier := range map[string]conversationpolicy.Tier{"full": conversationpolicy.Full, "cold": conversationpolicy.Cold} {
		rooms[name], err = st.EnsureConversation(source.ID, name, name)
		requirements.NoError(err)
		requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, name, tier))
	}
	sibling, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: rooms["full"], SourceMessageID: "sibling", MessageType: "beeper"})
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: rooms["full"], SourceMessageID: "moved", MessageType: "beeper"})
	requirements.NoError(err)
	requirements.NoError(st.SetEmbedGen(ctx, []int64{sibling}, 73))
	var before int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), sibling).Scan(&before))
	inFlight, err := st.CaptureConversationPolicyWork(ctx)
	requirements.NoError(err)

	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: rooms["cold"], SourceMessageID: "moved", MessageType: "beeper"})
	requirements.NoError(err)
	// Work started before the move cannot publish progress over cleanup's
	// discovery reset.
	requirements.ErrorIs(store.CheckConversationPolicyAdmission(inFlight), store.ErrConversationPolicyStale)
	var gen sql.NullInt64
	var revision int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT embed_gen,embedding_revision FROM messages WHERE id=?`), sibling).Scan(&gen, &revision))
	assertions.False(gen.Valid, "the sibling's context included the moved message")
	assertions.Greater(revision, before, "its existing vectors must stop matching")
}
