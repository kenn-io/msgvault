//go:build sqlite_vec

package embed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorker_RepairBelowWatermark_ReembedsAfterWatermarkReset is the
// regression guard for the below-watermark repair gap: repair-encoding clears
// embed_gen=NULL on a repaired message, but an INCREMENTAL embed run resumes
// from the per-gen watermark and only scans ids ABOVE it (ScanForEmbedding
// applies `id > watermark`). A repaired message whose id sits BELOW the current
// watermark is therefore never re-found by an incremental run — it would wait
// for a full-scan backstop (which the CLI defaults off and serve can have
// disabled).
//
// The fix lowers the watermark below the repaired id (Backend.ResetWatermarkBelow)
// so the next incremental RunOnce re-finds and re-embeds it. This test pins both
// halves of the gap:
//
//  1. WITHOUT the watermark reset, an incremental RunOnce after repair finds
//     NOTHING for the below-watermark repaired message (it stays missing) —
//     proving the gap the fix targets.
//  2. WITH the watermark reset (the new path), the next incremental RunOnce
//     re-embeds the repaired message (Succeeded>=1, missing==0).
func TestWorker_RepairBelowWatermark_ReembedsAfterWatermarkReset(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	// Seed 5 messages and embed all of them. With BatchSize 5 the worker scans
	// 1..5, embeds them, and advances the watermark to 5 (batchMax).
	f := newWorkerFixture(t, 5)
	w := newTestWorker(f, 5)
	res, err := w.RunOnce(ctx, f.BuildingGen)
	require.NoError(err, "initial RunOnce")
	require.Equal(5, res.Succeeded, "all 5 embedded")
	require.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "no missing after initial drain")
	require.Equal(int64(5), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)),
		"watermark advanced to the max embedded id")

	// Simulate repair-encoding on message 2 (BELOW the watermark of 5):
	//   - rewrite its body (the corrected text), AND
	//   - reset embed_gen to NULL (what store.Store.ResetEmbedGen does).
	// The body rewrite fires the trigger that bumps last_modified, mirroring a
	// real repair-encoding pass.
	const repairedID = 2
	_, err = f.MainDB.ExecContext(ctx,
		`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`,
		"repaired body 2 with corrected text", repairedID)
	require.NoError(err, "rewrite repaired body")
	_, err = f.MainDB.ExecContext(ctx,
		`UPDATE messages SET embed_gen = NULL WHERE id = ?`, repairedID)
	require.NoError(err, "reset embed_gen (ResetEmbedGen equivalent)")

	// The repaired message now reads as missing for the generation.
	require.Equal(1, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"repaired message reads as missing after embed_gen reset")

	// (1) WITHOUT the watermark reset: an incremental RunOnce resumes from the
	// watermark (5) and scans only id > 5, so it never re-finds message 2. The
	// gap the fix targets.
	gapWorker := newTestWorker(f, 5)
	gapRes, err := gapWorker.RunOnce(ctx, f.BuildingGen)
	require.NoError(err, "incremental RunOnce before watermark reset")
	assert.Equal(0, gapRes.Succeeded, "without the fix, the below-watermark repaired message is NOT re-found")
	assert.Equal(1, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"without the fix, the repaired message stays missing (waits for backstop)")

	// (2) WITH the fix: lower the watermark below the repaired id via the new
	// backend path, then run an incremental RunOnce. It now re-finds and
	// re-embeds message 2.
	require.NoError(f.Backend.ResetWatermarkBelow(ctx, repairedID),
		"ResetWatermarkBelow (the new repair path)")
	assert.Equal(int64(repairedID-1), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)),
		"watermark lowered to just below the repaired id")

	fixWorker := newTestWorker(f, 5)
	fixRes, err := fixWorker.RunOnce(ctx, f.BuildingGen)
	require.NoError(err, "incremental RunOnce after watermark reset")
	assert.GreaterOrEqual(fixRes.Succeeded, 1, "the repaired message is re-embedded after the watermark reset")
	assert.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"no missing after the fix re-embeds the repaired message")
}
