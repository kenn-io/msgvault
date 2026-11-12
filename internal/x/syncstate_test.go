package x

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXWalkContinuesPastTokenHistoryLimit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, time.Time{}, now)
	requirements.NoError(err)
	for i := range 1001 {
		walk, err = advanceWalk(walk, Page{NextToken: strconv.Itoa(i + 1)})
		requirements.NoError(err)
	}
	assertions.Len(walk.SeenTokens, 1000)
	assertions.Equal("1001", walk.Token)
	_, err = advanceWalk(walk, Page{NextToken: "1000"})
	requirements.ErrorIs(err, ErrRepeatedPageToken)
	walk, err = advanceWalk(walk, Page{})
	requirements.NoError(err)
	assertions.True(walk.Complete)
}

func TestXFrozenWalkRetainsBoundsAndCompleteWatermark(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 123, time.UTC)
	after := now.Add(-24 * time.Hour).Truncate(time.Second)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, after, now)
	_testRequire.NoError(err)
	_testAssert.Equal(now.Truncate(time.Second), walk.Before)
	first, err := advanceWalk(walk, Page{Posts: []Post{{ID: "102", CreatedAt: now.Add(-time.Minute)}}, NextToken: "page-two"})
	_testRequire.NoError(err)
	_testAssert.Empty(first.Watermark)
	_testAssert.False(first.Complete)
	_testAssert.Equal("102", first.MaxObservedID)
	resumed, err := prepareTimelineWalk("1001", "timeline", first, after, now.Add(3*time.Hour))
	_testRequire.NoError(err)
	_testAssert.Equal(first, resumed)
	last, err := advanceWalk(resumed, Page{Posts: []Post{{ID: "101", CreatedAt: now.Add(-2 * time.Minute)}}})
	_testRequire.NoError(err)
	_testAssert.True(last.Complete)
	_testAssert.Equal("102", last.Watermark)
	_testAssert.Empty(walk.Token, "progress helpers must not mutate the saved input")
	next, err := prepareTimelineWalk("1001", "timeline", last, after, now.Add(time.Hour))
	_testRequire.NoError(err)
	_testAssert.Equal("102", next.BaseWatermark)
	_testAssert.Equal("102", next.Watermark)
	req := timelineWalkRequest(next, 100)
	_testAssert.Equal("102", req.SinceID)
	_testAssert.True(req.After.IsZero(), "since_id cannot be combined with start_time")
	_testAssert.Equal(next.Before, req.Before)
}

func TestXWalkEmptyPagesAndTokenCycles(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "mentions", nil, time.Time{}, now)
	_testRequire.NoError(err)
	first, err := advanceWalk(walk, Page{NextToken: "second"})
	_testRequire.NoError(err)
	_testAssert.False(first.Complete)
	_testAssert.Equal("second", first.Token)
	_, err = advanceWalk(first, Page{NextToken: "second"})
	_testRequire.ErrorIs(err, ErrRepeatedPageToken)
	second, err := advanceWalk(first, Page{NextToken: "third"})
	_testRequire.NoError(err)
	_, err = advanceWalk(second, Page{NextToken: "second"})
	_testRequire.ErrorIs(err, ErrRepeatedPageToken)
	replayed := replayWalk(second)
	_testAssert.Equal(second.Fingerprint, replayed.Fingerprint)
	_testAssert.Equal(second.Before, replayed.Before)
	_testAssert.Empty(replayed.Token)
	_testAssert.Empty(replayed.SeenTokens)
	_testAssert.Equal("third", second.Token)
}

func TestXWalkItemObligationsPreventPublication(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, time.Time{}, now)
	_testRequire.NoError(err)
	partial, err := advanceWalk(walk, Page{Posts: []Post{{ID: "102", CreatedAt: now.Add(-time.Minute)}}, Errors: []ItemError{{ResourceType: "tweet", ID: "101", Code: "temporary"}}})
	_testRequire.NoError(err)
	_testAssert.Equal([]string{"101"}, partial.UnresolvedIDs)
	_testAssert.True(walkExhausted(partial))
	_testAssert.False(partial.Complete)
	_testAssert.Empty(partial.Watermark)
	resolved, err := resolveWalkItems(partial, []string{"101"})
	_testRequire.NoError(err)
	_testAssert.True(resolved.Complete)
	_testAssert.Equal("102", resolved.Watermark)
	_testAssert.Equal([]string{"101"}, partial.UnresolvedIDs)
	unknown, err := advanceWalk(walk, Page{NextToken: "next", Errors: []ItemError{{Code: "unidentified"}}})
	_testRequire.NoError(err)
	_testAssert.True(unknown.UnresolvedPage)
	_testAssert.Empty(unknown.Token, "an unidentified failure must replay the same page")
	_testAssert.False(walkExhausted(unknown))
	stillUnknown, err := resolveWalkItems(unknown, nil)
	_testRequire.NoError(err)
	_testAssert.True(stillUnknown.UnresolvedPage)
	_testAssert.False(stillUnknown.Complete)
	_testAssert.Empty(stillUnknown.Token)
	recovered, err := advanceWalk(unknown, Page{NextToken: "next"})
	_testRequire.NoError(err)
	_testAssert.False(recovered.UnresolvedPage)
	_testAssert.Equal("next", recovered.Token)
}

func TestXWalkChangedBoundsAndInvalidFingerprint(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	after := now.Add(-time.Hour)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, after, now)
	_testRequire.NoError(err)
	walk.Token = "opaque"
	walk.Watermark = "90"
	walk.BaseWatermark = "90"
	walk.Fingerprint = walkFingerprint(walk)
	changed, err := prepareTimelineWalk("1001", "timeline", walk, after.Add(-time.Hour), now.Add(time.Hour))
	_testRequire.NoError(err)
	_testAssert.Empty(changed.Token)
	_testAssert.Empty(changed.BaseWatermark)
	_testAssert.Equal("90", changed.Watermark)
	_testAssert.NotEqual(walk.Fingerprint, changed.Fingerprint)
	broken := *walk
	broken.Before = broken.Before.Add(time.Hour)
	_, err = prepareTimelineWalk("1001", "timeline", &broken, after, now)
	_testRequire.Error(err)
	_, err = prepareTimelineWalk("1002", "timeline", walk, after, now)
	_testRequire.Error(err)
	_, err = prepareTimelineWalk("01", "timeline", nil, after, now)
	_testRequire.Error(err)
	_, err = prepareTimelineWalk("1001", "unknown", nil, after, now)
	_testRequire.Error(err)
}

func TestXWalkIgnoresAboveCeilingAndBoundsObligations(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, now.Add(-time.Hour), now)
	_testRequire.NoError(err)
	finished, err := advanceWalk(walk, Page{Posts: []Post{{ID: "200", CreatedAt: now.Add(time.Minute)}, {ID: "100", CreatedAt: now.Add(-time.Minute)}, {ID: "50", CreatedAt: now.Add(-time.Hour)}}})
	_testRequire.NoError(err)
	_testAssert.Equal("100", finished.Watermark)
	full := *walk
	full.UnresolvedIDs = make([]string, store.XMaxUnresolvedIDs)
	for i := range full.UnresolvedIDs {
		full.UnresolvedIDs[i] = "101"
	}
	_, err = advanceWalk(&full, Page{Errors: []ItemError{{ResourceType: "tweet", ID: "102"}}})
	_testRequire.Error(err)
	_testAssert.Len(full.UnresolvedIDs, store.XMaxUnresolvedIDs)
}

func TestXWalkItemLookupPublishesItsObservedID(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, time.Time{}, now)
	_testRequire.NoError(err)
	partial, err := advanceWalk(walk, Page{Posts: []Post{{ID: "50", CreatedAt: now.Add(-time.Minute)}}, Errors: []ItemError{{ResourceType: "tweet", ID: "101", Code: "temporary"}}})
	_testRequire.NoError(err)
	resolved, err := resolveWalkItems(partial, []string{"101"}, Post{ID: "101", CreatedAt: now.Add(-time.Minute)})
	_testRequire.NoError(err)
	_testAssert.True(resolved.Complete)
	_testAssert.Equal("101", resolved.Watermark)
	_testAssert.Equal("50", partial.MaxObservedID)
}

func TestXOverlapFreezesBothEndpointBounds(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	own, err := prepareOverlapWalk("1001", nil, now)
	_testRequire.NoError(err)
	_testAssert.Equal("timeline", own.Endpoint)
	_testAssert.Equal(now.Add(-time.Hour), own.After)
	_testAssert.Equal(now, own.Before)
	complete, err := advanceWalk(own, Page{})
	_testRequire.NoError(err)
	mentions, err := nextOverlapEndpoint(complete)
	_testRequire.NoError(err)
	_testAssert.Equal("mentions", mentions.Endpoint)
	_testAssert.Equal(own.After, mentions.After)
	_testAssert.Equal(own.Before, mentions.Before)
	resumed, err := prepareOverlapWalk("1001", mentions, now.Add(6*time.Hour))
	_testRequire.NoError(err)
	_testAssert.Equal(mentions, resumed)
	finished, err := advanceWalk(mentions, Page{})
	_testRequire.NoError(err)
	next, err := prepareOverlapWalk("1001", finished, now.Add(6*time.Hour))
	_testRequire.NoError(err)
	_testAssert.Equal("timeline", next.Endpoint)
	_testAssert.Equal(now.Add(5*time.Hour), next.After)
	_testAssert.Empty(next.BaseWatermark)
}

func TestXTimelineReachRecordsPossibleGapWithoutBlockingExhaustion(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		reach    int64
	}{{"timeline", 3200}, {"mentions", 800}} {
		t.Run(tc.endpoint, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
			walk, err := prepareTimelineWalk("1001", tc.endpoint, nil, time.Time{}, now)
			_testRequire.NoError(err)
			walk.ObservedResults = tc.reach - 2
			first, err := advanceWalk(walk, Page{Posts: []Post{{ID: "110", CreatedAt: now.Add(-time.Minute)}}, NextToken: "last"})
			_testRequire.NoError(err)
			_testAssert.False(first.HistoryGapPossible)
			last, err := advanceWalk(first, Page{Posts: []Post{{ID: "109", CreatedAt: now.Add(-2 * time.Minute)}}})
			_testRequire.NoError(err)
			_testAssert.Equal(tc.reach, last.ObservedResults)
			_testAssert.True(last.HistoryGapPossible)
			_testAssert.True(last.Complete)
			_testAssert.Equal("110", last.Watermark)
			next, err := prepareTimelineWalk("1001", tc.endpoint, last, time.Time{}, now.Add(time.Hour))
			_testRequire.NoError(err)
			_testAssert.Zero(next.ObservedResults)
			_testAssert.True(next.HistoryGapPossible)
		})
	}
}
func TestXLongOutageMarksPossibleGapWithoutMovingFrozenBounds(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	walk, err := prepareTimelineWalk("1001", "mentions", nil, time.Time{}, now)
	_testRequire.NoError(err)
	resumed, err := prepareTimelineWalk("1001", "mentions", walk, time.Time{}, now.Add(25*time.Hour))
	_testRequire.NoError(err)
	_testAssert.True(resumed.HistoryGapPossible)
	_testAssert.Equal(now, resumed.Before)
	_testAssert.Equal(walk.Fingerprint, resumed.Fingerprint)
	completed, err := advanceWalk(walk, Page{})
	_testRequire.NoError(err)
	next, err := prepareTimelineWalk("1001", "mentions", completed, time.Time{}, now.Add(25*time.Hour))
	_testRequire.NoError(err)
	_testAssert.True(next.HistoryGapPossible)
}
