package visual

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestVisualStatusReportsConvergenceLagLeasesFormatsAndCostRisk(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	first := testVisualCandidate(t, f, "status-current", strings.Repeat("21", 32))
	second := testVisualCandidate(t, f, "status-pending", strings.Repeat("32", 32))
	unknownMessage := f.CreateMessage("status-unknown")
	unknownHash := strings.Repeat("43", 32)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), unknownMessage, store.AttachmentWrite{
		Filename: "unknown.png", MIMEType: "image/png", StoragePath: unknownHash[:2] + "/" + unknownHash,
		ContentHash: unknownHash, Size: 10, Role: store.AttachmentRoleUnknown,
		RoleSource: store.AttachmentRoleSourceUnknown, SourcePartKey: "part:1",
	}))
	data := encodedPNG(t, 2, 2)
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: data}, "visual-test/status")
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(result.Work, 2)
	for _, work := range result.Work {
		if work.Candidate.Owner.MessageID == first {
			publishReconciledWork(t, f, work)
		}
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "pending changed", second)
	require.NoError(err)
	usage := ProviderUsage{Requests: 7, InputBytes: 1234, BilledUnits: 2.5, UsageAvailable: true}

	status, err := reconciler.Status(t.Context(), usage, false, true)
	require.NoError(err)
	assert.Equal(int64(2), status.Eligible)
	assert.Equal(int64(1), status.Current)
	assert.Equal(int64(1), status.ActiveLeases)
	assert.Equal(int64(1), status.UnknownRole)
	// Context changes are swept from stale publications, not journaled, so
	// the drained journal reports no lag while convergence still blocks
	// activation.
	assert.Zero(status.JournalLag)
	assert.Equal(int64(1), status.Converged)
	assert.Equal(int64(2), status.ConvergenceTotal)
	assert.InDelta(0.5, status.ConvergenceRatio, 0.000001)
	assert.Equal(usage, status.Usage)
	assert.True(status.DuplicateCost.AtLeastOnce)
	assert.False(status.DuplicateCost.ProviderIdempotent)
	assert.Contains(status.DuplicateCost.Detail, "repeat a paid request")
	require.Len(status.Formats, 1)
	assert.Equal("image/png", status.Formats[0].MIMEType)
	assert.Equal(int64(2), status.Formats[0].Eligible)
	assert.Equal(int64(1), status.Formats[0].Current)
	assert.Equal(int64(len(data)*2), status.Formats[0].Bytes)
}

func TestVisualStatusReportsDurableTerminalAndRetryableOutcomes(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation := testVisualGeneration(t, f)
		testVisualCandidate(t, f, "status-terminal", strings.Repeat("54", 32))
		reconciler := testReconciler(t, f, generation.ID,
			memoryOpener{data: []byte("%PDF-1.7")}, "visual-test/status-terminal")
		_, err := reconciler.FullReconcile(t.Context())
		require.NoError(err)

		status, err := reconciler.Status(t.Context(), ProviderUsage{}, true, true)
		require.NoError(err)
		assert.Equal(int64(1), status.Terminal)
		assert.Zero(status.Stale, "a durable terminal outcome is not unresolved work")
		assert.Zero(status.Eligible)
		assert.InDelta(1.0, status.ConvergenceRatio, 0.000001)
		assert.False(status.DuplicateCost.AtLeastOnce)
		assert.True(status.DuplicateCost.ProviderIdempotent)
	})

	t.Run("retryable", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		generation := testVisualGeneration(t, f)
		testVisualCandidate(t, f, "status-retryable", strings.Repeat("65", 32))
		reconciler := testReconciler(t, f, generation.ID,
			memoryOpener{openErr: ErrContentUnavailable}, "visual-test/status-retryable")
		_, err := reconciler.FullReconcile(t.Context())
		require.NoError(err)

		status, err := reconciler.Status(t.Context(), ProviderUsage{}, false, true)
		require.NoError(err)
		assert.Equal(int64(1), status.Retryable)
		assert.Equal(int64(1), status.Unavailable)
		assert.Zero(status.Stale, "retryable outcomes are tracked in their own bucket")
	})
}

func TestVisualStatusHighWaterSurvivesJournalPruning(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := testVisualCandidate(t, f, "status-pruned", strings.Repeat("76", 32))
	reconciler := testReconciler(t, f, generation.ID,
		memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/status-pruned")
	initial, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(initial.Work, 1)
	publishReconciledWork(t, f, initial.Work[0])
	initial, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Empty(initial.Work)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "changed", messageID)
	require.NoError(err)
	replayed, err := reconciler.Replay(t.Context())
	require.NoError(err)
	require.Len(replayed.Work, 1)
	publishReconciledWork(t, f, replayed.Work[0])
	replayed, err = reconciler.Replay(t.Context())
	require.NoError(err)
	require.Empty(replayed.Work)

	status, err := reconciler.Status(t.Context(), ProviderUsage{}, false, true)
	require.NoError(err)
	assert.Equal(replayed.SourceFence, status.JournalHighWater)
	assert.Equal(replayed.SourceFence, status.JournalCursor)
	assert.Zero(status.JournalLag)
}

func TestStatusTreatsDurableOutcomesAsConvergedNotStale(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	current := testVisualCandidate(t, f, "durable-current", strings.Repeat("54", 32))
	terminalMessage := testVisualCandidate(t, f, "durable-terminal", strings.Repeat("65", 32))
	_ = terminalMessage

	// One candidate publishes; the other is a PDF and records a terminal
	// rejection.
	opener := routingOpener{
		strings.Repeat("54", 32): encodedPNG(t, 2, 2),
		strings.Repeat("65", 32): []byte("%PDF-1.7"),
	}
	reconciler := testReconciler(t, f, generation.ID, opener, "visual-test/durable")
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Len(result.Work, 1)
	assert.Equal(current, result.Work[0].Claim.Owner.MessageID)
	publishReconciledWork(t, f, result.Work[0])
	_, err = reconciler.FullReconcile(t.Context())
	require.NoError(err)

	// Full status: the terminal rejection is terminal, never stale, so the
	// activation gate can close.
	status, err := reconciler.Status(t.Context(), ProviderUsage{}, false, true)
	require.NoError(err)
	assert.Zero(status.Stale, "durable terminal outcomes must not hold activation open")
	assert.Equal(int64(1), status.Terminal)
	assert.Equal(int64(1), status.Current)

	// Light status skips the blob scan yet converges over evaluated owners.
	light, err := reconciler.Status(t.Context(), ProviderUsage{}, false, false)
	require.NoError(err)
	assert.Zero(light.Stale)
	assert.Zero(light.Retryable)
	assert.Equal(light.ConvergenceTotal, light.Converged,
		"without unresolved work the light status must report convergence")
	assert.Empty(light.Formats, "the light status must not read attachment blobs")
	assert.Zero(light.Eligible)
}

// routingOpener serves different bytes per blob hash.
type routingOpener map[string][]byte

func (o routingOpener) OpenStream(_ context.Context, hash string) (io.ReadCloser, int64, error) {
	data, ok := o[hash]
	if !ok {
		return nil, 0, ErrContentUnavailable
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func TestStatusBeforeFirstBuildReportsUnreconciledInsteadOfFailing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	testVisualCandidate(t, f, "status-fresh", strings.Repeat("54", 32))
	data := encodedPNG(t, 2, 2)
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: data}, "visual-test/status-fresh")

	// No FullReconcile has run, so the attachment-change consumer does not
	// exist yet. Status must describe the unbuilt generation, not fail.
	status, err := reconciler.Status(t.Context(), ProviderUsage{}, false, false)
	require.NoError(err)
	assert.Zero(status.JournalCursor)
	assert.Zero(status.Current)
	assert.Zero(status.Converged)
	assert.Equal(generation.ID, status.Generation.ID)
}

func TestStatusIgnoresRetryableOutcomeOnTombstonedOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	messageID := testVisualCandidate(t, f, "tombstoned-retryable", strings.Repeat("a7", 32))
	opener := &switchableOpener{inner: memoryOpener{openErr: ErrContentUnavailable}}
	reconciler := testReconciler(t, f, generation.ID, opener, "visual-test/tombstoned-retryable")
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	require.Equal(int64(1), result.Retryable)

	// Deleting the message tombstones the publication while its retryable
	// outcome remains recorded. Nothing live is left to retry, so status
	// must not count it — it would block activation forever.
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	status, err := reconciler.Status(t.Context(), ProviderUsage{}, false, false)
	require.NoError(err)
	assert.Zero(status.Retryable)
	assert.Equal(int64(1), status.Tombstoned)
}

func TestCapabilityChangeOnActiveGenerationReopensStatusCompleteness(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	for index, hash := range []string{strings.Repeat("f1", 32), strings.Repeat("f2", 32), strings.Repeat("f3", 32)} {
		testVisualCandidate(t, f, "cap-reopen-"+strconv.Itoa(index), hash)
	}
	data := encodedPNG(t, 2, 2)
	const consumerKey = "visual-test/cap-reopen"
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: data}, consumerKey)
	// Record the initial manifest fingerprint; only a CHANGE reopens.
	_, err := f.Store.SyncVisualGenerationCapabilityFingerprint(
		t.Context(), generation.ID, consumerKey, "capability-fp-initial")
	require.NoError(err)
	for {
		result, reconcileErr := reconciler.FullReconcile(t.Context())
		require.NoError(reconcileErr)
		if len(result.Work) == 0 {
			break
		}
		for _, work := range result.Work {
			publishReconciledWork(t, f, work)
		}
	}
	status, statusErr := reconciler.Status(t.Context(), ProviderUsage{}, false, false)
	require.NoError(statusErr)
	require.True(status.ReconciliationComplete)
	require.Equal(status.ConvergenceTotal, status.Converged)

	// A capability-manifest change reopens reconciliation on the active
	// generation and rebaselines the journal cursor. Every legacy counter
	// still reads "done" — zero lag, everything converged — so completeness
	// is the only signal keeping the build loop from exiting after one
	// bounded pass with most attachments unrescanned.
	changed, err := f.Store.SyncVisualGenerationCapabilityFingerprint(
		t.Context(), generation.ID, consumerKey, "capability-fp-next")
	require.NoError(err)
	require.True(changed)
	status, err = reconciler.Status(t.Context(), ProviderUsage{}, false, false)
	require.NoError(err)
	assert.False(status.ReconciliationComplete,
		"the reopened baseline must hold the build loop open")
	assert.Zero(status.JournalLag)
	assert.Equal(status.ConvergenceTotal, status.Converged,
		"legacy counters alone cannot distinguish the reopened scan")
}
