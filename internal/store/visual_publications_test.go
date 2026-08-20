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
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	requirements.NoError(f.Store.AdvanceVisualGenerationSourceFence(t.Context(), generation.ID, sourceFence))

	_, err := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, sourceFence-1)
	requirements.ErrorContains(err, "raced an attachment change")
	_, activateErr := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, sourceFence)
	requirements.NoError(activateErr)
}

func TestEnsureVisualGenerationRestartsRetiredPolicyWithoutConsent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
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

	t.Run("message text change marks current publication stale and sweepable", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation, owner, _ := publishVisualOwner(t, f, "visual-context")

		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET subject = ? WHERE id = ?`), "changed context", owner.MessageID)
		require.NoError(err)
		assertVisualPublicationState(t, f, generation.ID, owner, store.VisualPublicationStale)
		// Content changes do not enter the shared attachment journal; the
		// reconciler finds them through the stale-publication sweep.
		messageIDs, err := f.Store.ListStaleVisualMessageIDs(t.Context(), generation.ID, 10)
		require.NoError(err)
		assert.Equal([]int64{owner.MessageID}, messageIDs)
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

	t.Run("message body deletion marks current publication stale and sweepable", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation := createVisualGeneration(t, f)
		owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-body-delete")
		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`),
			owner.MessageID, "context that the embedding was built from")
		require.NoError(err)
		claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
		token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
			Claim: claim, RepresentativeAttachmentID: attachmentID,
			Role:       store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics,
		})
		require.NoError(err)
		require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, token))

		_, err = f.Store.DB().Exec(f.Store.Rebind(
			`DELETE FROM message_bodies WHERE message_id = ?`), owner.MessageID)
		require.NoError(err)
		assertVisualPublicationState(t, f, generation.ID, owner, store.VisualPublicationStale)
		messageIDs, err := f.Store.ListStaleVisualMessageIDs(t.Context(), generation.ID, 10)
		require.NoError(err)
		assert.Equal([]int64{owner.MessageID}, messageIDs,
			"an embedding built from deleted body text must be re-evaluated")
	})

	t.Run("context change clears durable outcome for re-evaluation", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation := createVisualGeneration(t, f)
		owner, _, sourceFence := createVisualOwner(t, f, "visual-outcome-clear")
		claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
		require.NoError(f.Store.RejectVisualPublication(t.Context(), claim, store.VisualOutcome{
			Kind: "terminal", Reason: "provider_rejected",
		}))

		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET subject = ? WHERE id = ?`), "new context", owner.MessageID)
		require.NoError(err)
		publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, owner)
		require.NoError(err)
		assert.Empty(publication.OutcomeKind,
			"a durable outcome must not survive the context it judged")
		messageIDs, err := f.Store.ListStaleVisualMessageIDs(t.Context(), generation.ID, 10)
		require.NoError(err)
		assert.Equal([]int64{owner.MessageID}, messageIDs)
	})

	t.Run("invalidation parks a cleared pending token for the sweep", func(t *testing.T) {
		require := require.New(t)
		f := storetest.New(t)
		generation := createVisualGeneration(t, f)
		owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-pending-park")
		claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
		token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
			Claim: claim, RepresentativeAttachmentID: attachmentID,
			Role:       store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics,
		})
		require.NoError(err)
		// A worker crash after PutUnpublished leaves a backend vector under
		// the pending token; the invalidation trigger must keep a durable
		// reference for the cleanup sweep when it clears the pending slot.
		_, err = f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE attachments SET width = ? WHERE id = ?`), 640, attachmentID)
		require.NoError(err)
		obsolete, err := f.Store.ListObsoleteVisualTokens(t.Context(), generation.ID, 100)
		require.NoError(err)
		require.Equal([]string{token}, obsolete)
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

	_, err := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, 0)
	requirements.ErrorContains(err, "explicit hosted-processing consent")

	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	activatedRetired, activateErr := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, 0)
	requirements.NoError(activateErr)
	requirements.Empty(activatedRetired)
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

func TestSyncVisualGenerationCapabilityFingerprintReopensReconciliation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	const consumerKey = "visual-test/capability-sync"
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), consumerKey)
	require.NoError(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(t.Context(), consumerKey, 0))

	// First sync records the fingerprint without owing a re-evaluation.
	changed, err := f.Store.SyncVisualGenerationCapabilityFingerprint(
		t.Context(), generation.ID, consumerKey, "capability-fp-1")
	require.NoError(err)
	assert.False(changed)
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), consumerKey)
	require.NoError(err)
	assert.True(consumer.ReconciliationComplete)

	// The same fingerprint is a no-op.
	changed, err = f.Store.SyncVisualGenerationCapabilityFingerprint(
		t.Context(), generation.ID, consumerKey, "capability-fp-1")
	require.NoError(err)
	assert.False(changed)

	// Journal activity after the first reconciliation: a retained entry
	// that must not read as "concurrent with" the reopened scan.
	owner, _, _ := createVisualOwner(t, f, "visual-capability-rebaseline")
	highWater, err := f.Store.AttachmentChangeHighWater(t.Context())
	require.NoError(err)
	require.Positive(highWater)
	require.Positive(owner.MessageID)

	// A re-probed manifest reopens reconciliation so every candidate is
	// re-evaluated under the new upload authority. The consumer is
	// rebaselined at the current high water: keeping the original baseline
	// would classify every retained historical journal entry as a
	// concurrent change and block the re-scan's commits forever.
	changed, err = f.Store.SyncVisualGenerationCapabilityFingerprint(
		t.Context(), generation.ID, consumerKey, "capability-fp-2")
	require.NoError(err)
	assert.True(changed)
	consumer, err = f.Store.GetAttachmentChangeConsumer(t.Context(), consumerKey)
	require.NoError(err)
	assert.False(consumer.ReconciliationComplete)
	assert.Equal(highWater, consumer.BaselineSequence,
		"reopening must rebaseline at the current journal high water")
}

func TestListAndPurgeRetiredVisualGenerations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-retired-purge")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, token))

	require.Error(f.Store.PurgeRetiredVisualGeneration(t.Context(), generation.ID),
		"a building generation must not be purged")
	retired, err := f.Store.ListRetiredVisualGenerations(t.Context())
	require.NoError(err)
	assert.Empty(retired)

	require.NoError(f.Store.RetireVisualGeneration(t.Context(), generation.ID))
	retired, err = f.Store.ListRetiredVisualGenerations(t.Context())
	require.NoError(err)
	require.Len(retired, 1)
	assert.Equal(generation.ID, retired[0].ID)
	assert.Equal(generation.Fingerprint, retired[0].Fingerprint)

	// Purge converges the cleanup sweep: once backend vectors are gone the
	// token listing must come back empty instead of re-listing forever.
	require.NoError(f.Store.PurgeRetiredVisualGeneration(t.Context(), generation.ID))
	tokens, err := f.Store.ListVisualGenerationTokens(t.Context(), generation.ID)
	require.NoError(err)
	assert.Empty(tokens)
}

func TestVisualSupersededTokensSurviveForSweep(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-superseded")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	firstToken, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, firstToken))

	// Re-embedding the same owner replaces the current token. The replaced
	// token must be listed for the obsolete sweep: the worker's inline
	// backend delete can fail, and the archive no longer references it.
	claim = claimVisualOwner(t, f, generation.ID, owner, "revision-2", sourceFence)
	secondToken, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NotEqual(firstToken, secondToken)
	require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, secondToken))

	obsolete, err := f.Store.ListObsoleteVisualTokens(t.Context(), generation.ID, 100)
	require.NoError(err)
	assert.Equal([]string{firstToken}, obsolete)
	all, err := f.Store.ListVisualGenerationTokens(t.Context(), generation.ID)
	require.NoError(err)
	assert.ElementsMatch([]string{firstToken, secondToken}, all)

	require.NoError(f.Store.ClearObsoleteVisualToken(t.Context(), generation.ID, firstToken))
	obsolete, err = f.Store.ListObsoleteVisualTokens(t.Context(), generation.ID, 100)
	require.NoError(err)
	assert.Empty(obsolete)
	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, owner)
	require.NoError(err)
	assert.Equal(secondToken, publication.CurrentVectorToken,
		"clearing the superseded token must not touch the live one")
}

func TestVisualDriftDiscardParksPendingTokenForSweep(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-drift-park")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	// A source change fires the invalidation trigger, which clears the
	// pending token reference before commit ever sees it. The worker's
	// inline backend delete is the remaining cleanup; when that delete
	// fails it parks the token so the sweep retries.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET width = ? WHERE id = ?`), 77, attachmentID)
	require.NoError(err)
	require.ErrorIs(f.Store.CommitVisualPublication(t.Context(), claim, token), store.ErrVisualSourceChanged)

	require.NoError(f.Store.ParkObsoleteVisualToken(t.Context(), generation.ID, owner, token))
	obsolete, err := f.Store.ListObsoleteVisualTokens(t.Context(), generation.ID, 100)
	require.NoError(err)
	require.Equal([]string{token}, obsolete)
	require.NoError(f.Store.ClearObsoleteVisualToken(t.Context(), generation.ID, token))
	obsolete, err = f.Store.ListObsoleteVisualTokens(t.Context(), generation.ID, 100)
	require.NoError(err)
	assert.Empty(obsolete)
}

func TestVisualPrepareParksLeftoverPendingToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-prepare-park")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	firstToken, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)

	// A crash between PutUnpublished and commit leaves the pending token
	// behind. The next prepare replaces it and must park the old token so
	// its possible backend vector is swept rather than orphaned.
	secondToken, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NotEqual(firstToken, secondToken)
	obsolete, err := f.Store.ListObsoleteVisualTokens(t.Context(), generation.ID, 100)
	require.NoError(err)
	assert.Equal([]string{firstToken}, obsolete)
}

func TestRestartRetiredGenerationRefusesLiveTokenReferences(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-restart-guard")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, token))
	require.NoError(f.Store.RetireVisualGeneration(t.Context(), generation.ID))

	// Retirement's backend token cleanup crashed: the ledger still holds the
	// only reference to the backend vector. Restarting must not delete it.
	spec := store.VisualGenerationSpec{
		Fingerprint: generation.Fingerprint, Model: generation.Model, Dimension: generation.Dimension,
	}
	_, err = f.Store.EnsureVisualGeneration(t.Context(), spec)
	require.ErrorIs(err, store.ErrVisualRetiredTokensRemain)

	// Once the backend vectors are deleted and the ledger purged, the
	// restart proceeds.
	require.NoError(f.Store.PurgeRetiredVisualGeneration(t.Context(), generation.ID))
	restarted, err := f.Store.EnsureVisualGeneration(t.Context(), spec)
	require.NoError(err)
	assert.Equal(store.VisualGenerationBuilding, restarted.State)
	assert.False(restarted.Consented)
}

func TestClaimRecordsSnapshotStampSoStaleContextCannotCommit(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-stamp-cas")
	snapshot, err := f.Store.GetVisualMessageContext(t.Context(), owner.MessageID)
	require.NoError(err)

	// The message is edited AFTER the context snapshot but BEFORE the claim.
	// A claim reading the live stamp would absorb the edit and let a vector
	// built from the stale snapshot commit as current. The stamp is bumped
	// explicitly because CURRENT_TIMESTAMP's one-second resolution can
	// otherwise leave a same-second edit undetectable.
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "edited mid-snapshot", owner.MessageID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET content_changed_at = ? WHERE id = ?`),
		time.Now().UTC().Add(time.Hour), owner.MessageID)
	require.NoError(err)

	claim, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID: generation.ID, Owner: owner, ProposedRevision: "revision-stale",
		LeaseOwner: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute,
		SourceFence:          sourceFence,
		ExpectedContentStamp: &snapshot.ContentStamp,
	})
	require.NoError(err)
	require.True(acquired)
	_, err = f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.ErrorIs(err, store.ErrVisualSourceChanged,
		"preparation must refuse a claim carrying a superseded snapshot stamp")
}

func TestPrepareRefusesContextChangeAfterClaim(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-prepare-fresh")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)

	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "edited before prepare", owner.MessageID)
	require.NoError(err)
	// Explicit stamp bump: a same-second edit is invisible at
	// CURRENT_TIMESTAMP resolution.
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET content_changed_at = ? WHERE id = ?`),
		time.Now().UTC().Add(time.Hour), owner.MessageID)
	require.NoError(err)
	_, err = f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.ErrorIs(err, store.ErrVisualSourceChanged,
		"the paid provider request must be skipped when the source moved after the claim")
}

func TestObsoleteTokenLedgerHoldsMultipleTokens(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := createVisualGeneration(t, f)
	owner, attachmentID, sourceFence := createVisualOwner(t, f, "visual-ledger-multi")
	claim := claimVisualOwner(t, f, generation.ID, owner, "revision-1", sourceFence)
	firstToken, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, firstToken))

	// A previously parked token (failed inline delete) must survive a
	// successor commit parking the replaced current token: the single-slot
	// design lost one of them.
	require.NoError(f.Store.ParkObsoleteVisualToken(t.Context(), generation.ID, owner, "orphaned-pending-token"))
	claim = claimVisualOwner(t, f, generation.ID, owner, "revision-2", sourceFence)
	secondToken, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(err)
	require.NoError(f.Store.CommitVisualPublication(t.Context(), claim, secondToken))

	obsolete, err := f.Store.ListObsoleteVisualTokens(t.Context(), generation.ID, 100)
	require.NoError(err)
	assert.ElementsMatch([]string{firstToken, "orphaned-pending-token"}, obsolete)
}
