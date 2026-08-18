package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestVisualWorkClaimLeaseAndFencingLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, _, _ := createVisualOwner(t, f, "visual-claim")
	now := time.Now().UTC()
	request := store.VisualClaimRequest{
		GenerationID: generation.ID, Owner: owner, ProposedRevision: "revision-1",
		LeaseOwner: "worker-a", Now: now, LeaseDuration: time.Minute, SourceFence: 7,
	}

	first, acquired, err := f.Store.ClaimVisualWork(t.Context(), request)
	require.NoError(err)
	assert.True(acquired)
	assert.Equal(int64(1), first.FencingToken)

	request.LeaseOwner = "worker-b"
	blocked, acquired, err := f.Store.ClaimVisualWork(t.Context(), request)
	require.NoError(err)
	assert.False(acquired)
	assert.Equal(first.FencingToken, blocked.FencingToken)
	assert.Equal("worker-a", blocked.LeaseOwner)

	request.Now = now.Add(2 * time.Minute)
	successor, acquired, err := f.Store.ClaimVisualWork(t.Context(), request)
	require.NoError(err)
	assert.True(acquired)
	assert.Equal(int64(2), successor.FencingToken)
	assert.Equal("worker-b", successor.LeaseOwner)

	err = f.Store.ReleaseVisualWork(t.Context(), first)
	require.ErrorIs(err, store.ErrVisualClaimLost)
	require.NoError(f.Store.RenewVisualWork(t.Context(), successor, request.Now.Add(30*time.Second), time.Minute))
	require.NoError(f.Store.ReleaseVisualWork(t.Context(), successor))

	request.Now = time.Now().UTC().Add(time.Second)
	reclaimed, acquired, err := f.Store.ClaimVisualWork(t.Context(), request)
	require.NoError(err)
	assert.True(acquired)
	assert.Equal(int64(3), reclaimed.FencingToken,
		"a released revision must not reuse an obsolete fencing token")
}

func TestVisualPublicationTwoPhaseCommitRejectsObsoleteFence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-publication")
	claim, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID: generation.ID, Owner: owner, ProposedRevision: "revision-1",
		LeaseOwner: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute,
		SourceFence: sourceFence,
	})
	require.NoError(err)
	require.True(acquired)

	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	assert.NotEmpty(token)

	// A source mutation after preparation invalidates the captured journal fence.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET width = ? WHERE id = ?`), 99, attachmentID)
	require.NoError(err)
	err = f.Store.CommitVisualPublication(t.Context(), claim, token)
	require.ErrorIs(err, store.ErrVisualSourceChanged)

	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationStale, publication.State)
	assert.Empty(publication.CurrentVectorToken)
}

func TestVisualPublicationCommitTerminalOutcomeAndTombstone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-states")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, token))

	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationCurrent, publication.State)
	assert.Equal(token, publication.CurrentVectorToken)
	assert.Equal("revision-1", publication.PublishedRevision)

	claim = claimVisualOwner(t, f, generation.ID, owner, "revision-2", sourceFence)
	require.NoError(f.Store.RejectVisualPublication(t.Context(), claim, store.VisualOutcome{
		Kind: "terminal", Reason: "provider_limit",
	}))
	publication, err = f.Store.GetVisualPublication(t.Context(), generation.ID, owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationStale, publication.State)
	assert.Equal(token, publication.CurrentVectorToken,
		"stale publications retain their obsolete token until backend cleanup")
	assert.Equal("terminal", publication.OutcomeKind)
	assert.Equal("provider_limit", publication.OutcomeReason)

	require.NoError(f.Store.TombstoneVisualOwner(t.Context(), generation.ID, owner, sourceFence+1))
	publication, err = f.Store.GetVisualPublication(t.Context(), generation.ID, owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationTombstoned, publication.State)
}

func TestVisualGenerationActivationRejectsStaleSourceFence(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	_, _, sourceFence := createVisualOwner(t, f, "visual-activation-race")
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID))
	requirements.NoError(f.Store.AdvanceVisualGenerationSourceFence(t.Context(), generation.ID, sourceFence))

	err := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, sourceFence-1)
	requirements.ErrorContains(err, "raced an attachment change")
	requirements.NoError(f.Store.ActivateVisualGeneration(t.Context(), generation.ID, sourceFence))
}

func TestEnsureVisualGenerationRestartsRetiredPolicyWithoutConsent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID))
	requirements.NoError(f.Store.RetireVisualGeneration(t.Context(), generation.ID))

	restarted := createVisualGeneration(t, f)
	assertions.Equal(generation.ID, restarted.ID)
	assertions.Equal(store.VisualGenerationBuilding, restarted.State)
	assertions.False(restarted.Consented)
}

func TestVisualSourceInvalidationIsSynchronous(t *testing.T) {
	t.Run("attachment metadata change marks current publication stale", func(t *testing.T) {
		f := storetest.New(t)
		generation, owner, attachmentID := publishVisualOwner(t, f, "visual-metadata")

		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE attachments SET width = ? WHERE id = ?`), 320, attachmentID)
		require.NoError(t, err)
		assertVisualPublicationState(t, f, generation.ID, owner, store.VisualPublicationStale)
	})

	t.Run("message text change marks current publication stale and journals it", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation, owner, _ := publishVisualOwner(t, f, "visual-context")

		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET subject = ? WHERE id = ?`), "changed context", owner.MessageID)
		require.NoError(err)
		assertVisualPublicationState(t, f, generation.ID, owner, store.VisualPublicationStale)
		changes, err := f.Store.ListAttachmentChanges(t.Context(), "visual-index/v1", 10)
		require.NoError(err)
		require.NotEmpty(changes)
		assert.Equal("message_content_change", changes[len(changes)-1].EventKind)
	})

	t.Run("message live exit tombstones current publication", func(t *testing.T) {
		f := storetest.New(t)
		generation, owner, _ := publishVisualOwner(t, f, "visual-delete")

		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), owner.MessageID)
		require.NoError(t, err)
		assertVisualPublicationState(t, f, generation.ID, owner, store.VisualPublicationTombstoned)
	})

	t.Run("last qualifying occurrence removal tombstones current publication", func(t *testing.T) {
		f := storetest.New(t)
		generation, owner, attachmentID := publishVisualOwner(t, f, "visual-role")

		_, err := f.Store.DB().Exec(f.Store.Rebind(`
			UPDATE attachments SET attachment_role = 'inline' WHERE id = ?`), attachmentID)
		require.NoError(t, err)
		assertVisualPublicationState(t, f, generation.ID, owner, store.VisualPublicationTombstoned)
	})

	t.Run("trigger effects roll back with the source mutation", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation, owner, attachmentID := publishVisualOwner(t, f, "visual-rollback")
		before := attachmentChangeCount(t, f)

		tx, err := f.Store.DB().BeginTx(t.Context(), nil)
		require.NoError(err)
		_, err = tx.Exec(f.Store.Rebind(
			`UPDATE attachments SET attachment_role = 'inline' WHERE id = ?`), attachmentID)
		require.NoError(err)
		require.NoError(tx.Rollback())

		assertVisualPublicationState(t, f, generation.ID, owner, store.VisualPublicationCurrent)
		assert.Equal(before, attachmentChangeCount(t, f))
	})
}

func TestVisualGenerationRequiresExplicitConsentBeforeActivation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	assertions.False(generation.Consented)

	err := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, 0)
	requirements.ErrorContains(err, "explicit hosted-processing consent")

	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID))
	requirements.NoError(f.Store.ActivateVisualGeneration(t.Context(), generation.ID, 0))
	active, err := f.Store.ActiveVisualGeneration(t.Context())
	requirements.NoError(err)
	assertions.True(active.Consented)
}

func createVisualGeneration(t *testing.T, f *storetest.Fixture) store.VisualGeneration {
	t.Helper()
	generation, err := f.Store.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "visual-fingerprint-v1", Model: "voyage-multimodal-3.5", Dimension: 1024,
	})
	require.NoError(t, err)
	return generation
}

func createVisualOwner(t *testing.T, f *storetest.Fixture, sourceMessageID string) (store.VisualOwner, int64, int64) {
	t.Helper()
	require := require.New(t)
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "visual-index/v1")
	require.NoError(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(t.Context(), consumer.ConsumerKey, consumer.BaselineSequence))
	messageID := f.CreateMessage(sourceMessageID)
	hash := strings.Repeat("a", 64)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "image.png", MIMEType: "image/png", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 10, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1",
	}))
	changes, err := f.Store.ListAttachmentChanges(t.Context(), consumer.ConsumerKey, 10)
	require.NoError(err)
	require.NotEmpty(changes)
	var attachmentID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentID))
	return store.VisualOwner{MessageID: messageID, BlobHash: hash, MediaInputKey: "original"}, attachmentID, changes[len(changes)-1].Sequence
}

func claimVisualOwner(t *testing.T, f *storetest.Fixture, generationID int64, owner store.VisualOwner, revision string, sourceFence int64) store.VisualWorkClaim {
	t.Helper()
	claim, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID: generationID, Owner: owner, ProposedRevision: revision,
		LeaseOwner: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: sourceFence,
	})
	require.NoError(t, err)
	require.True(t, acquired)
	return claim
}

func publishVisualOwner(
	t *testing.T,
	f *storetest.Fixture,
	sourceMessageID string,
) (store.VisualGeneration, store.VisualOwner, int64) {
	t.Helper()
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, sourceMessageID)
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(t, err)
	require.NoError(t, f.Store.CommitVisualPublication(t.Context(), claim, token))
	return generation, owner, attachmentID
}

func assertVisualPublicationState(
	t *testing.T,
	f *storetest.Fixture,
	generationID int64,
	owner store.VisualOwner,
	want store.VisualPublicationState,
) {
	t.Helper()
	publication, err := f.Store.GetVisualPublication(t.Context(), generationID, owner)
	require.NoError(t, err)
	assert.Equal(t, want, publication.State)
	if want != store.VisualPublicationCurrent {
		assert.NotEmpty(t, publication.CurrentVectorToken,
			"invalidating a published owner must retain its token for cleanup")
	}
}
