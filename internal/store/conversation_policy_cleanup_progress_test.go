package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
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
