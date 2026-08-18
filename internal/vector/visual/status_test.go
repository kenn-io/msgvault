package visual

import (
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

	status, err := reconciler.Status(t.Context(), usage, false)
	require.NoError(err)
	assert.Equal(int64(2), status.Eligible)
	assert.Equal(int64(1), status.Current)
	assert.Equal(int64(1), status.ActiveLeases)
	assert.Equal(int64(1), status.UnknownRole)
	assert.Positive(status.JournalLag)
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

		status, err := reconciler.Status(t.Context(), ProviderUsage{}, true)
		require.NoError(err)
		assert.Equal(int64(1), status.Terminal)
		assert.Equal(int64(1), status.Stale)
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

		status, err := reconciler.Status(t.Context(), ProviderUsage{}, false)
		require.NoError(err)
		assert.Equal(int64(1), status.Retryable)
		assert.Equal(int64(1), status.Unavailable)
		assert.Equal(int64(1), status.Stale)
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

	status, err := reconciler.Status(t.Context(), ProviderUsage{}, false)
	require.NoError(err)
	assert.Equal(replayed.SourceFence, status.JournalHighWater)
	assert.Equal(replayed.SourceFence, status.JournalCursor)
	assert.Zero(status.JournalLag)
}
