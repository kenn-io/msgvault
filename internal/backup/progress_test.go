package backup

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstAndFinal indexes events by stage: first[stage] is the index of that
// stage's first event, final[stage] is the last event marked Final.
func firstAndFinal(events []ProgressEvent) (first map[ProgressStage]int, final map[ProgressStage]ProgressEvent) {
	first = map[ProgressStage]int{}
	final = map[ProgressStage]ProgressEvent{}
	for i, ev := range events {
		if _, ok := first[ev.Stage]; !ok {
			first[ev.Stage] = i
		}
		if ev.Final {
			final[ev.Stage] = ev
		}
	}
	return first, final
}

func TestCreateEmitsProgressEventsInOrderWithFinalValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	var events []ProgressEvent
	opts := createOpts(dbPath, attachmentsDir, dataDir, t.TempDir())
	opts.Progress = func(ev ProgressEvent) { events = append(events, ev) }

	m, err := Create(context.Background(), r, newTestApp(), opts)
	require.NoError(err)
	require.NotEmpty(events)

	first, final := firstAndFinal(events)

	// Every stage fires, in freeze -> scan -> pack -> attachments -> seal
	// order. The pack stage is where a first backup spends its wall clock,
	// so it must report its own progress, not hide behind the scan bar.
	require.Contains(first, ProgressStageFreeze)
	require.Contains(first, ProgressStageScan)
	require.Contains(first, ProgressStagePack)
	require.Contains(first, ProgressStageAttachments)
	require.Contains(first, ProgressStageSeal)
	assert.Less(first[ProgressStageFreeze], first[ProgressStageScan],
		"freeze must start before scan")
	assert.Less(first[ProgressStageScan], first[ProgressStagePack],
		"scan must start before pack")
	assert.Less(first[ProgressStagePack], first[ProgressStageAttachments],
		"pack must start before attachments")
	assert.Less(first[ProgressStageAttachments], first[ProgressStageSeal],
		"attachments must start before seal")

	// The scan stage closes before the pack stage opens: page writing must
	// never render as a stalled scan bar.
	scanFinalIdx := -1
	for i, ev := range events {
		if ev.Stage == ProgressStageScan && ev.Final {
			scanFinalIdx = i
			break
		}
	}
	require.GreaterOrEqual(scanFinalIdx, 0)
	assert.Less(scanFinalIdx, first[ProgressStagePack])

	// Every stage reaches a Final event with Done == Total.
	require.Contains(final, ProgressStageFreeze)
	require.Contains(final, ProgressStageScan)
	require.Contains(final, ProgressStagePack)
	require.Contains(final, ProgressStageAttachments)
	require.Contains(final, ProgressStageSeal)

	freezeFinal := final[ProgressStageFreeze]
	assert.Equal(freezeFinal.Done, freezeFinal.Total)
	assert.Positive(freezeFinal.BytesDone, "freeze final reports the frozen DB size")
	assert.Equal(freezeFinal.BytesDone, freezeFinal.BytesTotal)

	scanFinal := final[ProgressStageScan]
	assert.Equal(scanFinal.Done, scanFinal.Total)
	assert.Equal(m.DB.PageCount, uint64(scanFinal.Total))

	attachFinal := final[ProgressStageAttachments]
	assert.Equal(attachFinal.Done, attachFinal.Total)
	assert.Equal(m.Attachments.Blobs, attachFinal.Done)
	assert.Equal(m.Attachments.BlobBytes, attachFinal.BytesDone)

	packFinal := final[ProgressStagePack]
	assert.Equal(packFinal.Done, packFinal.Total)
	assert.Positive(packFinal.Total, "a first backup stores every page")
	assert.Equal(packFinal.BytesDone, packFinal.BytesTotal)

	sealFinal := final[ProgressStageSeal]
	assert.Equal(int64(1), sealFinal.Done)
	assert.Equal(int64(1), sealFinal.Total)
}

// TestCreateNoChangeSkipsPackStage pins that a snapshot with no dirty pages
// emits no pack-stage events: there is nothing to write, so a zero-length
// bar would only be noise.
func TestCreateNoChangeSkipsPackStage(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	var events []ProgressEvent
	opts := createOpts(dbPath, attachmentsDir, dataDir, cacheDir)
	opts.Progress = func(ev ProgressEvent) { events = append(events, ev) }
	_, err = Create(ctx, r, newTestApp(), opts)
	require.NoError(err)
	for _, ev := range events {
		require.NotEqual(ProgressStagePack, ev.Stage,
			"a no-change snapshot has no pages to store")
	}
}

func TestCreateProgressNilCallbackIsSilent(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	opts := createOpts(dbPath, attachmentsDir, dataDir, t.TempDir())
	require.Nil(opts.Progress)

	_, err := Create(context.Background(), r, newTestApp(), opts)
	require.NoError(err)
}

func TestVerifyEmitsProgressEventsWithFinalValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	r, _ := buildVerifyFixture(t)

	var events []ProgressEvent
	res, err := Verify(context.Background(), r, newTestApp(), VerifyOptions{
		Progress: func(ev ProgressEvent) { events = append(events, ev) },
	})
	require.NoError(err)
	require.Empty(res.Problems)
	require.NotEmpty(events)

	for _, ev := range events {
		assert.Equal(ProgressStageVerify, ev.Stage)
	}

	// Full mode's bar tracks content blobs, not snapshots: the first event
	// announces the drain's total queued blob count at zero done, and Done
	// then climbs monotonically to that total.
	first := events[0]
	assert.Zero(first.Done)
	assert.Positive(first.Total, "the drain announces how many content blobs it will read")
	prev := int64(-1)
	for _, ev := range events {
		assert.GreaterOrEqual(ev.Done, prev, "blob progress must be monotonic")
		prev = ev.Done
	}

	var sawBlobRead bool
	for _, ev := range events {
		if ev.BytesDone > 0 {
			sawBlobRead = true
			break
		}
	}
	assert.True(sawBlobRead, "at least one event must report bytes read from a verified blob")

	last := events[len(events)-1]
	assert.True(last.Final)
	assert.Equal(last.Done, last.Total)
	assert.Equal(first.Total, last.Total,
		"the final event closes out the same drain denominator the bar advanced with")
	assert.Equal(res.BytesRead, last.BytesDone)
}

func TestVerifyProgressNilCallbackIsSilent(t *testing.T) {
	require := require.New(t)
	r, _ := buildVerifyFixture(t)

	res, err := Verify(context.Background(), r, newTestApp(), VerifyOptions{})
	require.NoError(err)
	require.Empty(res.Problems)
}
