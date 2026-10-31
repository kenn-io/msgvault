package x

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXFrozenWalkRetainsBoundsAndCompleteWatermark(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 123, time.UTC)
	after := now.Add(-24 * time.Hour).Truncate(time.Second)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, after, now)
	require.NoError(t, err)
	assert.Equal(t, now.Truncate(time.Second), walk.Before)
	first, err := advanceWalk(walk, Page{Posts: []Post{{ID: "102", CreatedAt: now.Add(-time.Minute)}}, NextToken: "page-two"})
	require.NoError(t, err)
	assert.Empty(t, first.Watermark)
	assert.False(t, first.Complete)
	assert.Equal(t, "102", first.MaxObservedID)
	resumed, err := prepareTimelineWalk("1001", "timeline", first, after, now.Add(3*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, first, resumed)
	last, err := advanceWalk(resumed, Page{Posts: []Post{{ID: "101", CreatedAt: now.Add(-2 * time.Minute)}}})
	require.NoError(t, err)
	assert.True(t, last.Complete)
	assert.Equal(t, "102", last.Watermark)
	assert.Empty(t, walk.Token, "progress helpers must not mutate the saved input")
	next, err := prepareTimelineWalk("1001", "timeline", last, after, now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, "102", next.BaseWatermark)
	assert.Equal(t, "102", next.Watermark)
	req := timelineWalkRequest(next, 100)
	assert.Equal(t, "102", req.SinceID)
	assert.True(t, req.After.IsZero(), "since_id cannot be combined with start_time")
	assert.Equal(t, next.Before, req.Before)
}

func TestXWalkEmptyPagesAndTokenCycles(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "mentions", nil, time.Time{}, now)
	require.NoError(t, err)
	first, err := advanceWalk(walk, Page{NextToken: "second"})
	require.NoError(t, err)
	assert.False(t, first.Complete)
	assert.Equal(t, "second", first.Token)
	_, err = advanceWalk(first, Page{NextToken: "second"})
	assert.ErrorIs(t, err, ErrRepeatedPageToken)
	second, err := advanceWalk(first, Page{NextToken: "third"})
	require.NoError(t, err)
	_, err = advanceWalk(second, Page{NextToken: "second"})
	assert.ErrorIs(t, err, ErrRepeatedPageToken)
	replayed := replayWalk(second)
	assert.Equal(t, second.Fingerprint, replayed.Fingerprint)
	assert.Equal(t, second.Before, replayed.Before)
	assert.Empty(t, replayed.Token)
	assert.Empty(t, replayed.SeenTokens)
	assert.Equal(t, "third", second.Token)
}

func TestXWalkItemObligationsPreventPublication(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, time.Time{}, now)
	require.NoError(t, err)
	partial, err := advanceWalk(walk, Page{Posts: []Post{{ID: "102", CreatedAt: now.Add(-time.Minute)}}, Errors: []ItemError{{ID: "101", Code: "temporary"}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"101"}, partial.UnresolvedIDs)
	assert.True(t, walkExhausted(partial))
	assert.False(t, partial.Complete)
	assert.Empty(t, partial.Watermark)
	resolved, err := resolveWalkItems(partial, []string{"101"})
	require.NoError(t, err)
	assert.True(t, resolved.Complete)
	assert.Equal(t, "102", resolved.Watermark)
	assert.Equal(t, []string{"101"}, partial.UnresolvedIDs)
	unknown, err := advanceWalk(walk, Page{NextToken: "next", Errors: []ItemError{{Code: "unidentified"}}})
	require.NoError(t, err)
	assert.True(t, unknown.UnresolvedPage)
	assert.Empty(t, unknown.Token, "an unidentified failure must replay the same page")
	assert.False(t, walkExhausted(unknown))
	_, err = resolveWalkItems(unknown, nil)
	assert.ErrorIs(t, err, ErrUnresolvedPage)
	recovered, err := advanceWalk(unknown, Page{NextToken: "next"})
	require.NoError(t, err)
	assert.False(t, recovered.UnresolvedPage)
	assert.Equal(t, "next", recovered.Token)
}

func TestXWalkChangedBoundsAndInvalidFingerprint(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	after := now.Add(-time.Hour)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, after, now)
	require.NoError(t, err)
	walk.Token = "opaque"
	walk.Watermark = "90"
	walk.BaseWatermark = "90"
	walk.Fingerprint = walkFingerprint(walk)
	changed, err := prepareTimelineWalk("1001", "timeline", walk, after.Add(-time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, changed.Token)
	assert.Empty(t, changed.BaseWatermark)
	assert.Equal(t, "90", changed.Watermark)
	assert.NotEqual(t, walk.Fingerprint, changed.Fingerprint)
	broken := *walk
	broken.Before = broken.Before.Add(time.Hour)
	_, err = prepareTimelineWalk("1001", "timeline", &broken, after, now)
	assert.Error(t, err)
	_, err = prepareTimelineWalk("1002", "timeline", walk, after, now)
	assert.Error(t, err)
	_, err = prepareTimelineWalk("01", "timeline", nil, after, now)
	assert.Error(t, err)
	_, err = prepareTimelineWalk("1001", "unknown", nil, after, now)
	assert.Error(t, err)
}

func TestXWalkIgnoresAboveCeilingAndBoundsObligations(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, now.Add(-time.Hour), now)
	require.NoError(t, err)
	finished, err := advanceWalk(walk, Page{Posts: []Post{{ID: "200", CreatedAt: now.Add(time.Minute)}, {ID: "100", CreatedAt: now.Add(-time.Minute)}, {ID: "50", CreatedAt: now.Add(-time.Hour)}}})
	require.NoError(t, err)
	assert.Equal(t, "100", finished.Watermark)
	full := *walk
	full.UnresolvedIDs = make([]string, store.XMaxUnresolvedIDs)
	for i := range full.UnresolvedIDs {
		full.UnresolvedIDs[i] = "101"
	}
	_, err = advanceWalk(&full, Page{Errors: []ItemError{{ID: "102"}}})
	assert.Error(t, err)
	assert.Len(t, full.UnresolvedIDs, store.XMaxUnresolvedIDs)
}

func TestXWalkItemLookupPublishesItsObservedID(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, time.Time{}, now)
	require.NoError(t, err)
	partial, err := advanceWalk(walk, Page{Posts: []Post{{ID: "50", CreatedAt: now.Add(-time.Minute)}}, Errors: []ItemError{{ID: "101", Code: "temporary"}}})
	require.NoError(t, err)
	resolved, err := resolveWalkItems(partial, []string{"101"}, Post{ID: "101", CreatedAt: now.Add(-time.Minute)})
	require.NoError(t, err)
	assert.True(t, resolved.Complete)
	assert.Equal(t, "101", resolved.Watermark)
	assert.Equal(t, "50", partial.MaxObservedID)
}

func TestXOverlapFreezesBothEndpointBounds(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	own, err := prepareOverlapWalk("1001", nil, now)
	require.NoError(t, err)
	assert.Equal(t, "timeline", own.Endpoint)
	assert.Equal(t, now.Add(-time.Hour), own.After)
	assert.Equal(t, now, own.Before)
	complete, err := advanceWalk(own, Page{})
	require.NoError(t, err)
	mentions, err := nextOverlapEndpoint(complete)
	require.NoError(t, err)
	assert.Equal(t, "mentions", mentions.Endpoint)
	assert.Equal(t, own.After, mentions.After)
	assert.Equal(t, own.Before, mentions.Before)
	resumed, err := prepareOverlapWalk("1001", mentions, now.Add(6*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, mentions, resumed)
	finished, err := advanceWalk(mentions, Page{})
	require.NoError(t, err)
	next, err := prepareOverlapWalk("1001", finished, now.Add(6*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, "timeline", next.Endpoint)
	assert.Equal(t, now.Add(5*time.Hour), next.After)
	assert.Empty(t, next.BaseWatermark)
}

func TestXTimelineReachRecordsPossibleGapWithoutBlockingExhaustion(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		reach    int64
	}{{"timeline", 3200}, {"mentions", 800}} {
		t.Run(tc.endpoint, func(t *testing.T) {
			now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
			walk, err := prepareTimelineWalk("1001", tc.endpoint, nil, time.Time{}, now)
			require.NoError(t, err)
			walk.ObservedResults = tc.reach - 2
			first, err := advanceWalk(walk, Page{Posts: []Post{{ID: "110", CreatedAt: now.Add(-time.Minute)}}, NextToken: "last"})
			require.NoError(t, err)
			assert.False(t, first.HistoryGapPossible)
			last, err := advanceWalk(first, Page{Posts: []Post{{ID: "109", CreatedAt: now.Add(-2 * time.Minute)}}})
			require.NoError(t, err)
			assert.Equal(t, tc.reach, last.ObservedResults)
			assert.True(t, last.HistoryGapPossible)
			assert.True(t, last.Complete)
			assert.Equal(t, "110", last.Watermark)
			next, err := prepareTimelineWalk("1001", tc.endpoint, last, time.Time{}, now.Add(time.Hour))
			require.NoError(t, err)
			assert.Zero(t, next.ObservedResults)
			assert.True(t, next.HistoryGapPossible)
		})
	}
}
func TestXLongOutageMarksPossibleGapWithoutMovingFrozenBounds(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "mentions", nil, time.Time{}, now)
	require.NoError(t, err)
	resumed, err := prepareTimelineWalk("1001", "mentions", walk, time.Time{}, now.Add(25*time.Hour))
	require.NoError(t, err)
	assert.True(t, resumed.HistoryGapPossible)
	assert.Equal(t, now, resumed.Before)
	assert.Equal(t, walk.Fingerprint, resumed.Fingerprint)
	completed, err := advanceWalk(walk, Page{})
	require.NoError(t, err)
	next, err := prepareTimelineWalk("1001", "mentions", completed, time.Time{}, now.Add(25*time.Hour))
	require.NoError(t, err)
	assert.True(t, next.HistoryGapPossible)
}
