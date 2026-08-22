package visual

import (
	"context"
	"errors"
	"io"
	"strconv"
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
	// Context changes never enter the shared attachment journal; the stale
	// sweep produced the work, so the journal cursor is already drained.
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "visual-test/replay")
	require.NoError(err)
	assert.Equal(consumer.BaselineSequence, consumer.LastSequence)
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
		MediaPolicy: fullyAuthorizedPolicy(), ContextPolicy: ContextPolicy{
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

func TestReconcileEnforcesAccountScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	inScope := testVisualCandidate(t, f, "scope-in", strings.Repeat("aa", 32))

	// A second account with its own attachment must stay outside the lane.
	otherSource, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	otherMessage, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: f.ConvID, SourceID: otherSource.ID,
		SourceMessageID: "scope-out", MessageType: "email", SizeEstimate: 1000,
	})
	require.NoError(err)
	outHash := strings.Repeat("bb", 32)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), otherMessage, store.AttachmentWrite{
		Filename: "synthetic.png", MIMEType: "image/png", StoragePath: outHash[:2] + "/" + outHash,
		ContentHash: outHash, Size: 10, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1",
	}))

	reconciler, err := NewReconciler(f.Store, memoryOpener{data: encodedPNG(t, 2, 2)}, ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: "visual-test/scope", PageSize: 10,
		LeaseOwner: "test-worker", LeaseDuration: time.Minute,
		SourceIDs:   []int64{f.Source.ID},
		MediaPolicy: fullyAuthorizedPolicy(), ContextPolicy: ContextPolicy{
			MaxChars: 4000, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1",
		},
	})
	require.NoError(err)
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1)
	assert.Equal(inScope, result.Work[0].Claim.Owner.MessageID,
		"attachments from accounts outside [vector.multimodal.scope] must never be scheduled for upload")
}

func TestReplayDoesNotConsumePagesWhileForeignClaimsAreActive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := testVisualCandidate(t, f, "foreign-claim", strings.Repeat("cc", 32))
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/foreign")

	initial, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(initial.Work, 1)
	publishReconciledWork(t, f, initial.Work[0])
	_, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)

	// Another worker claims re-embedding work for a different revision and
	// then goes silent while a role change journals an event.
	foreign, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID:     generation.ID,
		Owner:            store.VisualOwner{MessageID: messageID, BlobHash: strings.Repeat("cc", 32), MediaInputKey: "original"},
		ProposedRevision: "foreign-revision", LeaseOwner: "another-worker",
		LeaseDuration: time.Hour, SourceFence: 0, Now: time.Now().UTC(),
	})
	require.NoError(err)
	require.True(acquired)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET filename = ? WHERE message_id = ?`), "renamed.png", messageID)
	require.NoError(err)

	before, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "visual-test/foreign")
	require.NoError(err)
	_, err = reconciler.Replay(t.Context())
	require.NoError(err)
	after, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "visual-test/foreign")
	require.NoError(err)
	assert.Equal(before.LastSequence, after.LastSequence,
		"a journal page must not be consumed while another worker's claim is active")

	require.NoError(f.Store.ReleaseVisualWork(t.Context(), foreign))
	_, err = reconciler.Replay(t.Context())
	require.NoError(err)
	released, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "visual-test/foreign")
	require.NoError(err)
	assert.Greater(released.LastSequence, before.LastSequence,
		"the page is consumed once the foreign claim is gone and its owner re-evaluated")
}

func TestTerminalProviderOutcomeConvergesInsteadOfReclaiming(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	testVisualCandidate(t, f, "terminal-converge", strings.Repeat("61", 32))
	data := encodedPNG(t, 2, 2)
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: data}, "visual-test/terminal")

	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1)
	// The worker records a durable terminal provider decision for this
	// exact document revision (e.g. the provider refused the media).
	require.NoError(f.Store.RejectVisualPublication(t.Context(), result.Work[0].Claim, store.VisualOutcome{
		Kind: "terminal", Reason: "provider_rejected",
	}))

	// The next pass must treat it as converged — re-claiming would resubmit
	// and re-bill the same media forever and hold reconciliation open.
	result, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.Empty(result.Work)
	assert.Equal(int64(1), result.Terminal)
	needsFull, err := reconciler.NeedsFullReconcile(t.Context())
	require.NoError(err)
	assert.False(needsFull, "reconciliation must complete over a durable terminal outcome")

	// A context change produces a new revision and re-opens exactly that owner.
	messageIDs, err := f.Store.ListStaleVisualMessageIDs(t.Context(), generation.ID, 10)
	require.NoError(err)
	assert.Empty(messageIDs, "a matching terminal outcome is not sweepable work")
}

func TestReconcilePassBoundsClaimedOwnersNotMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	// One message with three standalone attachments: the per-pass bound
	// must hold per owner, not per message page.
	messageID := f.CreateMessage("owner-cap")
	for index, hash := range []string{strings.Repeat("71", 32), strings.Repeat("72", 32), strings.Repeat("73", 32)} {
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: "synthetic.png", MIMEType: "image/png", StoragePath: hash[:2] + "/" + hash,
			ContentHash: hash, Size: 10, Role: store.AttachmentRoleStandalone,
			RoleSource:    store.AttachmentRoleSourceImporterSemantics,
			SourcePartKey: "part:" + strconv.Itoa(index+1),
		}))
	}
	data := encodedPNG(t, 2, 2)
	reconciler, err := NewReconciler(f.Store, memoryOpener{data: data}, ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: "visual-test/owner-cap", PageSize: 2,
		LeaseOwner: "test-worker", LeaseDuration: time.Minute,
		MediaPolicy: fullyAuthorizedPolicy(), ContextPolicy: ContextPolicy{
			MaxChars: 4000, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1",
		},
	})
	require.NoError(err)

	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.Len(result.Work, 2, "a pass claims at most PageSize owners")
	for _, work := range result.Work {
		publishReconciledWork(t, f, work)
	}
	result, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.Len(result.Work, 1, "the remaining owner is claimed on the next pass")
}

func TestRetryOwnerBypassesTerminalConvergence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := testVisualCandidate(t, f, "terminal-retry", strings.Repeat("81", 32))
	data := encodedPNG(t, 2, 2)
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: data}, "visual-test/terminal-retry")

	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1)
	require.NoError(f.Store.RejectVisualPublication(t.Context(), result.Work[0].Claim, store.VisualOutcome{
		Kind: "terminal", Reason: "provider_rejected",
	}))
	// Reconciliation treats the durable decision as converged...
	result, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Empty(result.Work)

	// ...but an explicit operator retry must clear it and re-claim.
	result, err = reconciler.RetryOwner(t.Context(), messageID, strings.Repeat("81", 32))
	require.NoError(err)
	assert.Len(result.Work, 1, "retry of a terminal outcome must produce fresh work")
}

type switchableOpener struct{ inner StreamOpener }

func (o *switchableOpener) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	return o.inner.OpenStream(ctx, hash)
}

func TestRetryableOutcomeRecoversWhenTransientConditionClears(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	testVisualCandidate(t, f, "retryable-recover", strings.Repeat("91", 32))
	opener := &switchableOpener{inner: memoryOpener{openErr: ErrContentUnavailable}}
	reconciler := testReconciler(t, f, generation.ID, opener, "visual-test/retryable-recover")

	// The blob is temporarily unavailable: reconciliation records a
	// retryable outcome and completes its baseline over it.
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.Empty(result.Work)
	assert.Equal(int64(1), result.Retryable)
	needsFull, err := reconciler.NeedsFullReconcile(t.Context())
	require.NoError(err)
	require.False(needsFull)

	// Once the blob is back, the stale sweep must reconsider the owner —
	// otherwise the generation could never reach activation.
	opener.inner = memoryOpener{data: encodedPNG(t, 2, 2)}
	result, err = reconciler.Replay(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1, "a recovered retryable owner must be re-claimed by the sweep")
	publishReconciledWork(t, f, result.Work[0])
	result, err = reconciler.Replay(t.Context())
	require.NoError(err)
	assert.Empty(result.Work)
}

type countingOpener struct {
	data  []byte
	opens map[string]int
}

func (o *countingOpener) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	o.opens[hash]++
	return memoryOpener{data: o.data}.OpenStream(ctx, hash)
}

func TestFullReconcileResumesScanInsteadOfRescanningProcessedBlobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	hashes := []string{strings.Repeat("b1", 32), strings.Repeat("b2", 32), strings.Repeat("b3", 32)}
	for index, hash := range hashes {
		testVisualCandidate(t, f, "cursor-"+strconv.Itoa(index), hash)
	}
	opener := &countingOpener{data: encodedPNG(t, 2, 2), opens: map[string]int{}}
	reconciler, err := NewReconciler(f.Store, opener, ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: "visual-test/scan-cursor", PageSize: 1,
		LeaseOwner: "test-worker", LeaseDuration: time.Minute,
		MediaPolicy: fullyAuthorizedPolicy(), ContextPolicy: ContextPolicy{
			MaxChars: 4000, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1",
		},
	})
	require.NoError(err)

	// Three bounded passes, publishing each pass's work before the next.
	for range hashes {
		result, err := reconciler.FullReconcile(t.Context())
		require.NoError(err)
		require.Len(result.Work, 1)
		publishReconciledWork(t, f, result.Work[0])
	}
	// Each pass resumes from the page it claimed, re-inspecting at most that
	// page; a scan restarting at zero would reread the first blob on every
	// pass and make large builds quadratic in blob IO.
	assert.LessOrEqual(opener.opens[hashes[0]], 2,
		"the first blob must not be re-read by every later pass")
	assert.Equal(1, opener.opens[hashes[2]],
		"the frontier blob is inspected exactly once")

	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.Empty(result.Work, "the scan completes after the last owner publishes")
}

func TestMessageTypeScopeEntryIsDiscoveredWithoutRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	// The message starts outside the type-scoped lane (fixture messages are
	// 'email'; the lane only covers 'chat').
	messageID := testVisualCandidate(t, f, "scope-entry", strings.Repeat("c1", 32))
	reconciler, err := NewReconciler(f.Store, memoryOpener{data: encodedPNG(t, 2, 2)}, ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: "visual-test/scope-entry", PageSize: 10,
		MessageTypes: []string{"chat"},
		LeaseOwner:   "test-worker", LeaseDuration: time.Minute,
		MediaPolicy: fullyAuthorizedPolicy(), ContextPolicy: ContextPolicy{
			MaxChars: 4000, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1",
		},
	})
	require.NoError(err)
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Empty(result.Work, "out-of-scope message yields no work")

	// A re-import corrects the message type into scope. No attachment
	// journal event fires, so the seeded placeholder is the only signal;
	// the sweep must discover and index the owner without a full rebuild.
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET message_type = 'chat' WHERE id = ?`), messageID)
	require.NoError(err)
	result, err = reconciler.Replay(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1, "the scope-entry placeholder must reach the sweep")
	assert.Equal(messageID, result.Work[0].Candidate.Owner.MessageID)
	publishReconciledWork(t, f, result.Work[0])

	// Scope exit tombstones the publication...
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET message_type = 'email' WHERE id = ?`), messageID)
	require.NoError(err)
	result, err = reconciler.Replay(t.Context())
	require.NoError(err)
	require.Empty(result.Work)

	// ...and RE-ENTRY must revive it: the seed's conflict clause upserts
	// the tombstoned row back to stale. The retained vector token and
	// unchanged revision then let the sweep RESTORE the publication in
	// place — searchable again with zero provider work, no rebuild.
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET message_type = 'chat' WHERE id = ?`), messageID)
	require.NoError(err)
	result, err = reconciler.Replay(t.Context())
	require.NoError(err)
	assert.Empty(result.Work, "the retained vector restores without provider work")
	publication := visualPublicationForMessage(t, f, generation.ID, messageID)
	assert.Equal(store.VisualPublicationCurrent, publication.State,
		"scope re-entry must make the attachment searchable again")
}

func TestFullReconcilePassIsPageBounded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	for index, hash := range []string{strings.Repeat("e1", 32), strings.Repeat("e2", 32), strings.Repeat("e3", 32)} {
		testVisualCandidate(t, f, "page-bound-"+strconv.Itoa(index), hash)
	}
	opener := &countingOpener{data: encodedPNG(t, 2, 2), opens: map[string]int{}}
	reconciler, err := NewReconciler(f.Store, opener, ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: "visual-test/page-bound",
		PageSize: 1, MaxScanPagesPerPass: 1,
		LeaseOwner: "test-worker", LeaseDuration: time.Minute,
		MediaPolicy: fullyAuthorizedPolicy(), ContextPolicy: ContextPolicy{
			MaxChars: 4000, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1",
		},
	})
	require.NoError(err)

	// One page per pass: each invocation inspects at most PageSize owners
	// even when it finds work, and the baseline stays open until the scan
	// genuinely reaches the end.
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1)
	total := 0
	for _, count := range opener.opens {
		total += count
	}
	assert.Equal(1, total, "a bounded pass must not scan past its page cap")
	needsFull, err := reconciler.NeedsFullReconcile(t.Context())
	require.NoError(err)
	assert.True(needsFull, "the baseline must stay open after a bounded pass")
}

func TestPersistentProviderRetryableDoesNotLoopTheBaseline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	first := testVisualCandidate(t, f, "retry-loop-0", strings.Repeat("91", 32))
	second := testVisualCandidate(t, f, "retry-loop-1", strings.Repeat("92", 32))
	data := encodedPNG(t, 2, 2)
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: data}, "visual-test/retry-loop")

	// Pass one claims both owners; the provider persistently fails the
	// first (429/5xx), recorded as a durable retryable outcome, while the
	// second publishes.
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(result.Work, 2)
	for _, work := range result.Work {
		if work.Candidate.Owner.MessageID == first {
			require.NoError(f.Store.RejectVisualPublication(t.Context(), work.Claim, store.VisualOutcome{
				Kind: OutcomeRetryable, Reason: "provider_error",
			}))
			continue
		}
		publishReconciledWork(t, f, work)
	}

	// The next baseline pass must treat the durable retryable as evaluated:
	// re-claiming it immediately would hammer the failing provider on every
	// pass with the cursor pinned, starving all later owners. The baseline
	// completes; the post-completion sweep owns the retry.
	result, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.Empty(result.Work, "the baseline must not re-claim a durable retryable")
	assert.Equal(int64(1), result.Retryable)
	needsFull, err := reconciler.NeedsFullReconcile(t.Context())
	require.NoError(err)
	assert.False(needsFull, "the baseline completes over the retryable outcome")
	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID,
		store.VisualOwner{MessageID: second, BlobHash: strings.Repeat("92", 32), MediaInputKey: "original"})
	require.NoError(err)
	assert.Equal(store.VisualPublicationCurrent, publication.State,
		"the later owner published instead of being starved")

	// Post-completion, the stale sweep re-claims it for a bounded retry.
	result, err = reconciler.Replay(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1)
	assert.Equal(first, result.Work[0].Candidate.Owner.MessageID)
}
