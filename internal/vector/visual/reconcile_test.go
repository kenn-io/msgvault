package visual

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestFullReconcileClaimsExactRevisionAndCompletesBaseline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	first := testVisualCandidate(t, f, "reconcile-first", strings.Repeat("a1", 32))
	second := testVisualCandidate(t, f, "reconcile-second", strings.Repeat("b2", 32))
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/full")
	reconciler.config.PageSize = 1

	var workItems []WorkItem
	var sourceFence int64
	for {
		result, err := reconciler.FullReconcile(t.Context())
		require.NoError(err)
		sourceFence = result.SourceFence
		if len(result.Work) == 0 {
			break
		}
		workItems = append(workItems, result.Work...)
		for _, work := range result.Work {
			publishReconciledWork(t, f, work)
		}
	}
	require.Len(workItems, 2)
	assert.ElementsMatch([]int64{first, second}, []int64{
		workItems[0].Claim.Owner.MessageID, workItems[1].Claim.Owner.MessageID,
	})
	for _, work := range workItems {
		assert.Equal(work.Document.Revision, work.Claim.ProposedRevision)
		assert.Equal(sourceFence, work.Claim.SourceFence)
		assert.Equal(work.Candidate.Owner, work.Claim.Owner)
	}
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "visual-test/full")
	require.NoError(err)
	assert.True(consumer.ReconciliationComplete)
	assert.Equal(consumer.BaselineSequence, sourceFence)
}

func TestReplayReclaimsMessageTextRevisionThenTombstonesRoleChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := testVisualCandidate(t, f, "reconcile-edit", strings.Repeat("c3", 32))
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/replay")

	initial, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(initial.Work, 1)
	publishReconciledWork(t, f, initial.Work[0])
	completed, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Empty(completed.Work)
	initialRevision := initial.Work[0].Document.Revision

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "changed synthetic subject", messageID)
	require.NoError(err)
	replayed, err := reconciler.Replay(t.Context())
	require.NoError(err)
	require.Len(replayed.Work, 1)
	assert.NotEqual(initialRevision, replayed.Work[0].Document.Revision)
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "visual-test/replay")
	require.NoError(err)
	assert.Less(consumer.LastSequence, replayed.SourceFence)
	publishReconciledWork(t, f, replayed.Work[0])
	_, err = reconciler.Replay(t.Context())
	require.NoError(err)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET attachment_role = 'inline' WHERE message_id = ?`), messageID)
	require.NoError(err)
	_, err = reconciler.Replay(t.Context())
	require.NoError(err)
	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, replayed.Work[0].Candidate.Owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationTombstoned, publication.State)
}

func TestReconcilePersistsTerminalAndRetryableEligibilityOutcomes(t *testing.T) {
	t.Run("terminal format", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation := testVisualGeneration(t, f)
		messageID := testVisualCandidate(t, f, "reconcile-terminal", strings.Repeat("d4", 32))
		reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: []byte("%PDF-1.7")}, "visual-test/terminal")

		result, err := reconciler.FullReconcile(t.Context())
		require.NoError(err)
		assert.Equal(int64(1), result.Terminal)
		assert.Empty(result.Work)
		publication := visualPublicationForMessage(t, f, generation.ID, messageID)
		assert.Equal("terminal", publication.OutcomeKind)
		assert.Equal(string(ReasonUnsupportedMediaType), publication.OutcomeReason)
	})

	t.Run("temporarily unavailable content", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation := testVisualGeneration(t, f)
		messageID := testVisualCandidate(t, f, "reconcile-retryable", strings.Repeat("e5", 32))
		reconciler := testReconciler(t, f, generation.ID,
			memoryOpener{openErr: ErrContentUnavailable}, "visual-test/retryable")

		result, err := reconciler.FullReconcile(t.Context())
		require.NoError(err)
		assert.Equal(int64(1), result.Retryable)
		publication := visualPublicationForMessage(t, f, generation.ID, messageID)
		assert.Equal("retryable", publication.OutcomeKind)
		assert.Equal(string(ReasonContentUnavailable), publication.OutcomeReason)
	})
}

func TestReplayDoesNotAdvanceCursorBeforeDurableDecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := testVisualCandidate(t, f, "reconcile-cursor", strings.Repeat("f6", 32))
	consumerKey := "visual-test/cursor"
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, consumerKey)
	initial, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(initial.Work, 1)
	publishReconciledWork(t, f, initial.Work[0])
	completed, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Empty(completed.Work)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "new subject", messageID)
	require.NoError(err)
	before, err := f.Store.GetAttachmentChangeConsumer(t.Context(), consumerKey)
	require.NoError(err)
	failing := testReconciler(t, f, generation.ID,
		memoryOpener{openErr: errors.New("synthetic storage outage")}, consumerKey)

	_, err = failing.Replay(t.Context())
	require.ErrorContains(err, "synthetic storage outage")
	after, err := f.Store.GetAttachmentChangeConsumer(t.Context(), consumerKey)
	require.NoError(err)
	assert.Equal(before.LastSequence, after.LastSequence)
}

func TestReplayRestoresSameRevisionWithoutProviderWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := testVisualCandidate(t, f, "reconcile-preserve", strings.Repeat("ab", 32))
	reconciler := testReconciler(t, f, generation.ID,
		memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/preserve")

	initial, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(initial.Work, 1)
	publishReconciledWork(t, f, initial.Work[0])
	initial, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Empty(initial.Work)
	before := visualPublicationForMessage(t, f, generation.ID, messageID)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET filename = ? WHERE message_id = ?`), "renamed.png", messageID)
	require.NoError(err)
	stale := visualPublicationForMessage(t, f, generation.ID, messageID)
	require.Equal(store.VisualPublicationStale, stale.State)

	replayed, err := reconciler.Replay(t.Context())
	require.NoError(err)
	assert.Empty(replayed.Work)
	assert.Equal(int64(1), replayed.AlreadyCurrent)
	after := visualPublicationForMessage(t, f, generation.ID, messageID)
	assert.Equal(store.VisualPublicationCurrent, after.State)
	assert.Equal(before.CurrentVectorToken, after.CurrentVectorToken)
	assert.Equal(before.PublishedRevision, after.PublishedRevision)
}

func TestFullReconcileRepairsDeliberatelyMissedEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := f.CreateMessage("reconcile-missed")
	hash := strings.Repeat("17", 32)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.png", MIMEType: "image/png", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 10, Role: store.AttachmentRoleInline,
		RoleSource: store.AttachmentRoleSourceMIMEDisposition, SourcePartKey: "part:1",
	}))
	consumerKey := "visual-test/missed"
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, consumerKey)
	initial, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.Empty(initial.Work)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET attachment_role = 'standalone' WHERE message_id = ?`), messageID)
	require.NoError(err)
	changes, err := f.Store.ListAttachmentChanges(t.Context(), consumerKey, 10)
	require.NoError(err)
	require.NotEmpty(changes)
	require.NoError(f.Store.AdvanceAttachmentChangeConsumer(
		t.Context(), consumerKey, changes[len(changes)-1].Sequence,
	), "simulate an event acknowledged without its decision")

	repaired, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(repaired.Work, 1)
	assert.Equal(messageID, repaired.Work[0].Candidate.Owner.MessageID)
}

func testVisualGeneration(t *testing.T, f *storetest.Fixture) store.VisualGeneration {
	t.Helper()
	generation, err := f.Store.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "visual-reconcile-fingerprint", Model: "voyage-multimodal-3.5", Dimension: 1024,
	})
	require.NoError(t, err)
	return generation
}

func testVisualCandidate(t *testing.T, f *storetest.Fixture, sourceID, hash string) int64 {
	t.Helper()
	messageID := f.CreateMessage(sourceID)
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.png", MIMEType: "image/png", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 10, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1",
	}))
	return messageID
}

func testReconciler(
	t *testing.T,
	f *storetest.Fixture,
	generationID int64,
	opener StreamOpener,
	consumerKey string,
) *Reconciler {
	t.Helper()
	reconciler, err := NewReconciler(f.Store, opener, ReconcileConfig{
		GenerationID: generationID, ConsumerKey: consumerKey, PageSize: 10,
		LeaseOwner: "test-worker", LeaseDuration: time.Minute,
		MediaPolicy: DefaultMediaPolicy(), ContextPolicy: ContextPolicy{
			MaxChars: 4000, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1",
		},
	})
	require.NoError(t, err)
	return reconciler
}

func publishReconciledWork(t *testing.T, f *storetest.Fixture, work WorkItem) {
	t.Helper()
	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: work.Claim, RepresentativeAttachmentID: work.Candidate.RepresentativeAttachmentID,
		Role: work.Candidate.Role, RoleSource: work.Candidate.RoleSource,
	})
	require.NoError(t, err)
	require.NoError(t, f.Store.CommitVisualPublication(t.Context(), work.Claim, token))
}

func visualPublicationForMessage(
	t *testing.T,
	f *storetest.Fixture,
	generationID, messageID int64,
) store.VisualPublication {
	t.Helper()
	page, err := f.Store.ListVisualPublications(t.Context(), generationID, store.VisualPublicationFilter{
		MessageIDs: []int64{messageID},
	})
	require.NoError(t, err)
	require.Len(t, page.Publications, 1)
	return page.Publications[0]
}
