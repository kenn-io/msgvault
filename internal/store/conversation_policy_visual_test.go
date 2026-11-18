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

func TestVisualColdPolicyRejectsOldProgressAndClaim(t *testing.T) {
	requirements := require.New(t)

	f := storetest.New(t)
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
