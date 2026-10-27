package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
	"time"
)

func TestDocumentColdPolicySharedFullSupport(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f, gen := seedDocumentVectorGenerationWithChunks(t, 1)
	ctx := t.Context()
	candidates, err := f.Store.ListDocumentVectorChunkCandidates(ctx, gen.ID, 0, 10)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	hash := candidates[0].CanonicalBlobHash
	fullRoom, err := f.Store.EnsureConversation(f.Source.ID, "full-room", "Full room")
	requirements.NoError(err)
	f.ConvID = fullRoom
	fullMessage := f.CreateMessage("shared-full-document")
	requirements.NoError(f.Store.UpsertAttachmentRecord(ctx, fullMessage, store.AttachmentWrite{Filename: "shared.pdf", MIMEType: "application/pdf", Size: 128, StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition, SourcePartKey: "mime:1"}))
	attachmentID := singleAttachmentID(t, f, fullMessage)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(ctx, attachmentID, 2)
	requirements.NoError(err)
	requirements.True(eligible)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	candidates, err = f.Store.ListDocumentVectorChunkCandidates(ctx, gen.ID, 0, 10)
	requirements.NoError(err)
	requirements.Len(candidates, 1, "a later Full owner supports the shared hash")
	claim, err := f.Store.ClaimDocumentVectorChunk(ctx, gen.ID, 0, 10, "policy-worker", time.Now().UTC(), time.Minute)
	requirements.NoError(err)
	requirements.NotNil(claim)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "full-room", conversationpolicy.Cold))
	candidates, err = f.Store.ListDocumentVectorChunkCandidates(ctx, gen.ID, 0, 10)
	requirements.NoError(err)
	assertions.Empty(candidates, "last Full owner demotion revokes semantic input")
	requirements.Error(f.Store.CommitDocumentVectorPublication(ctx, gen.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, time.Now().UTC()))
	var count int
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM document_chunks`).Scan(&count))
	assertions.Equal(1, count, "extracted text stays archived")
}

func TestVisualColdPolicyNeverUsesColdContext(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := storetest.New(t)
	ctx := t.Context()
	gen := createVisualGeneration(t, f)
	owner, attachment, fence := createVisualOwner(t, f, "policy-visual")
	claim := claimVisualOwner(t, f, gen.ID, owner, "policy-revision", fence)
	token, err := f.Store.PrepareVisualPublication(ctx, store.PreparedVisualPublication{Claim: claim, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
	requirements.NoError(err)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	_, err = f.Store.GetVisualMessageContext(ctx, owner.MessageID)
	requirements.Error(err, "cold message context cannot enter provider input")
	page, err := f.Store.ListVisualCandidates(ctx, store.VisualCandidateFilter{LimitMessages: 10})
	requirements.NoError(err)
	assertions.Empty(page.Candidates)
	requirements.Error(f.Store.CommitVisualPublication(ctx, claim, token))
}

func TestDocumentColdPolicyRevokedTokenNotReused(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f, gen := seedDocumentVectorGenerationWithChunks(t, 1)
	ctx := t.Context()
	now := time.Now().UTC()
	claim, err := f.Store.ClaimDocumentVectorChunk(ctx, gen.ID, 0, 10, "old-worker", now, time.Minute)
	requirements.NoError(err)
	requirements.NotNil(claim)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	page, err := f.Store.ParkObsoleteDocumentVectorTokens(ctx, gen.ID, "", 10, now.Add(2*time.Minute))
	requirements.NoError(err)
	requirements.Len(page.Tokens, 1)
	finalized, err := f.Store.FinalizeObsoleteDocumentVectorToken(ctx, gen.ID, claim.Token, now.Add(2*time.Minute))
	requirements.NoError(err)
	requirements.True(finalized)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	fresh, err := f.Store.ClaimDocumentVectorChunk(ctx, gen.ID, 0, 10, "new-worker", now.Add(3*time.Minute), time.Minute)
	requirements.NoError(err)
	requirements.NotNil(fresh)
	assertions.NotEqual(claim.Token, fresh.Token, "revoked token cannot name the recreated publication")
	retry, err := f.Store.ClaimDocumentVectorChunk(ctx, gen.ID, 0, 10, "new-worker", now.Add(3*time.Minute), time.Minute)
	requirements.NoError(err)
	requirements.NotNil(retry)
	assertions.Equal(fresh.Token, retry.Token, "current incarnation retries stay idempotent")
	tx, err := f.Store.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	requirements.ErrorIs(f.Store.CheckConversationPolicyDocumentTokensTx(ctx, tx, gen.ID, []string{claim.Token}), store.ErrConversationPolicyStale)
	requirements.NoError(tx.Rollback())
}

func TestDocumentVisualColdPolicyRejectsOldProgressAndClaim(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f, gen := seedDocumentVectorGenerationWithChunks(t, 1)
	f.Store.DB().SetMaxOpenConns(1)
	ctx := t.Context()
	visualGen := createVisualGeneration(t, f)
	owner, _, fence := createVisualOwner(t, f, "progress-owner")
	liveClaim := claimVisualOwner(t, f, visualGen.ID, owner, "before-transition", fence)
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(ctx, "visual-test/policy-progress")
	requirements.NoError(err)
	original, err := f.Store.CaptureConversationPolicyWork(ctx)
	requirements.NoError(err)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	requirements.ErrorIs(f.Store.CheckpointDocumentVectorBuildForFingerprint(original, gen.ID, gen.Fingerprint, 100, false, store.DocumentVectorUsageDelta{}, time.Now().UTC()), store.ErrConversationPolicyStale)
	cursor, err := f.Store.GetDocumentVectorBuildCursor(ctx, gen.ID)
	requirements.NoError(err)
	assertions.Zero(cursor)
	requirements.ErrorIs(f.Store.ActivateDocumentVectorGeneration(original, gen.ID, time.Now().UTC()), store.ErrConversationPolicyStale)
	requirements.ErrorIs(f.Store.CompleteAttachmentChangeReconciliation(original, consumer.ConsumerKey, consumer.BaselineSequence), store.ErrConversationPolicyStale)
	requirements.ErrorIs(f.Store.AdvanceVisualGenerationSourceFence(original, visualGen.ID, fence), store.ErrConversationPolicyStale)
	requirements.ErrorIs(f.Store.TombstoneVisualOwner(original, visualGen.ID, owner, fence), store.ErrConversationPolicyStale)
	requirements.ErrorIs(f.Store.RejectVisualPublication(original, liveClaim, store.VisualOutcome{Kind: "terminal", Reason: "old-outcome"}), store.ErrConversationPolicyStale)
	_, err = f.Store.ActivateVisualGeneration(original, visualGen.ID, fence)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	_, _, err = f.Store.ClaimVisualWork(original, store.VisualClaimRequest{GenerationID: visualGen.ID, Owner: owner, ProposedRevision: "original", LeaseOwner: "old", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: fence})
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	requirements.NoError(f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	_, _, err = f.Store.ClaimVisualWork(ctx, store.VisualClaimRequest{GenerationID: visualGen.ID, Owner: owner, ProposedRevision: "cold", LeaseOwner: "cold", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: fence})
	requirements.Error(err)
}
