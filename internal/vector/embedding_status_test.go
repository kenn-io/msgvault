package vector

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingStatusRecentRateIncludesFailuresAndActiveAge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	s := EmbeddingStatus{Pending: 90, Job: EmbeddingJobState{State: "running"}, Diagnostics: &EmbeddingDiagnostics{
		GenerationID: 1,
		RecentBatches: []EmbeddingBatch{
			{StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(-45 * time.Second), Completed: 10},
			{StartedAt: now.Add(-45 * time.Second), FinishedAt: now.Add(-30 * time.Second), Completed: 0},
		},
	}}
	s.Generation.ID = 1
	s.SetRecentRate(now, false, EmbeddingRecentBatchLimit)
	require.NotNil(s.MessagesPerMinute)
	assert.InDelta(float64(10), *s.MessagesPerMinute, 0.0001)
	require.NotNil(s.ETASeconds)
	assert.InDelta(float64(540), *s.ETASeconds, 0.0001)
	assert.Equal(2, s.WindowBatches)
	s.SetRecentRate(now, true, EmbeddingRecentBatchLimit)
	assert.Nil(s.ETASeconds, "filtered coverage cannot use generation-wide speed")
	s.Job.State = "idle"
	s.SetRecentRate(now, false, EmbeddingRecentBatchLimit)
	assert.Nil(s.ETASeconds)
	s.Job.State = "running"
	s.SetRecentRate(now.Add(10*time.Minute), false, EmbeddingRecentBatchLimit)
	assert.Nil(s.MessagesPerMinute, "old samples do not predict live progress")
}

func FuzzEmbeddingStatusRateFinite(f *testing.F) {
	f.Add(int64(0), int64(0), int64(0))
	f.Add(int64(32), int64(15), int64(855000))
	f.Fuzz(func(t *testing.T, completed, seconds, pending int64) {
		completed %= 1000000
		seconds %= 3600
		pending = max(pending%10000000, 0)
		now := time.Unix(10000, 0).UTC()
		s := EmbeddingStatus{Pending: pending, Job: EmbeddingJobState{State: "running"}, Diagnostics: &EmbeddingDiagnostics{
			RecentBatches: []EmbeddingBatch{{StartedAt: now.Add(-time.Duration(seconds) * time.Second), FinishedAt: now, Completed: int(completed)}},
		}}
		s.SetRecentRate(now, false, EmbeddingRecentBatchLimit)
		if s.MessagesPerMinute != nil {
			assert.False(t, math.IsNaN(*s.MessagesPerMinute))
			assert.False(t, math.IsInf(*s.MessagesPerMinute, 0))
			assert.GreaterOrEqual(t, *s.MessagesPerMinute, float64(0))
		}
		if s.ETASeconds != nil {
			assert.False(t, math.IsInf(*s.ETASeconds, 0))
			assert.GreaterOrEqual(t, *s.ETASeconds, float64(0))
		}
	})
}
