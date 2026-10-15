package vector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetRecentRate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	s := EmbeddingStatus{CoverageSampledAt: now, Pending: 90, Job: EmbeddingJobState{State: "running"}, Diagnostics: &EmbeddingDiagnostics{
		GenerationID: 1,
		RecentBatches: []EmbeddingBatch{
			{StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(-45 * time.Second), Completed: 10},
			{StartedAt: now.Add(-45 * time.Second), FinishedAt: now.Add(-30 * time.Second), Completed: 0},
		},
	}}
	s.Generation.ID = 1
	s.SetRecentRate(now, false, EmbeddingRecentBatchLimit)
	require.NotNil(s.MessagesPerMinute)
	assert.InDelta(float64(20), *s.MessagesPerMinute, 0.0001)
	require.NotNil(s.ETASeconds)
	assert.InDelta(float64(270), *s.ETASeconds, 0.0001)
	assert.Equal(2, s.WindowBatches)
	assert.InDelta(float64(30), s.WindowSeconds, 0.0001)
	s.SetRecentRate(now.Add(time.Minute), false, EmbeddingRecentBatchLimit)
	require.NotNil(s.MessagesPerMinute)
	assert.InDelta(float64(20), *s.MessagesPerMinute, 0.0001, "idle time does not change the completed-batch rate")
	s.CoverageSampledAt = now.Add(-EmbeddingCoverageMaxAge - time.Second)
	s.SetRecentRate(now, false, EmbeddingRecentBatchLimit)
	assert.Nil(s.ETASeconds, "stale pending counts cannot predict remaining time")
	require.NotNil(s.MessagesPerMinute, "coverage age does not change batch throughput")
	assert.InDelta(float64(20), *s.MessagesPerMinute, 0.0001)
	s.CoverageSampledAt = now
	s.SetRecentRate(now, true, EmbeddingRecentBatchLimit)
	assert.Nil(s.ETASeconds, "filtered coverage cannot use generation-wide speed")
	s.Job.State = "idle"
	s.SetRecentRate(now, false, EmbeddingRecentBatchLimit)
	assert.Nil(s.ETASeconds)
	s.Job.State = "running"
	s.SetRecentRate(now.Add(10*time.Minute), false, EmbeddingRecentBatchLimit)
	assert.Nil(s.MessagesPerMinute, "old samples do not predict live progress")
}
