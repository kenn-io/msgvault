package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateWindow_EmptyReturnsZero(t *testing.T) {
	w := newRateWindow(10)
	require.Zero(t, w.Rate(), "empty Rate")
	require.Zero(t, w.Samples(), "empty Samples")
}

func TestRateWindow_PartialFill(t *testing.T) {
	w := newRateWindow(10)
	w.Add(50, 1*time.Second)
	w.Add(100, 1*time.Second)
	require.Equal(t, 2, w.Samples(), "Samples")
	// sum(msgs)/sum(seconds) = 150/2 = 75
	require.InDelta(t, 75.0, w.Rate(), 0.01, "Rate")
}

func TestRateWindow_EvictsOldestOnceFull(t *testing.T) {
	w := newRateWindow(3)
	// Fill with low-rate samples.
	w.Add(10, 1*time.Second) // 10 msg/s
	w.Add(10, 1*time.Second)
	w.Add(10, 1*time.Second)
	require.InDelta(t, 10.0, w.Rate(), 0.01, "pre-eviction Rate")
	// Push a high-rate sample; oldest 10/1 should fall out.
	w.Add(1000, 1*time.Second)
	// Now window holds three samples: 10, 10, 1000 over 3s -> 340 msg/s.
	require.Equal(t, 3, w.Samples(), "Samples after eviction")
	require.InDelta(t, 340.0, w.Rate(), 0.01, "post-eviction Rate")
}

func TestRateWindow_WeightedNotMeanOfPerBatchRates(t *testing.T) {
	// Picking sizes that distinguish weighted from mean-of-rates.
	//   Batch A: 1 msg in 10s   -> 0.1 msg/s
	//   Batch B: 100 msgs in 1s -> 100 msg/s
	//   Mean of rates: 50.05
	//   Weighted: 101 / 11 = ~9.18
	w := newRateWindow(2)
	w.Add(1, 10*time.Second)
	w.Add(100, 1*time.Second)
	require.InDelta(t, 101.0/11.0, w.Rate(), 0.01, "weighted Rate (must NOT equal mean-of-rates ~50)")
}

func TestRateWindow_ZeroElapsedSampleSkipped(t *testing.T) {
	w := newRateWindow(10)
	w.Add(10, 1*time.Second)
	w.Add(50, 0) // skipped: would divide by zero
	w.Add(20, 1*time.Second)
	require.Equal(t, 2, w.Samples(), "Samples (zero-elapsed should be skipped)")
	// 30 msgs over 2s = 15.
	require.InDelta(t, 15.0, w.Rate(), 0.01, "Rate")
}

func TestRateWindow_NegativeElapsedSampleSkipped(t *testing.T) {
	w := newRateWindow(10)
	w.Add(10, 1*time.Second)
	w.Add(50, -1*time.Second) // pathological: skip
	require.Equal(t, 1, w.Samples(), "Samples")
	// 10 msgs over 1s = 10 — confirms the negative sample didn't slip
	// into the running totals (a regression where it did would either
	// pull totalElapsed to zero or leave a stale msg count behind).
	require.InDelta(t, 10.0, w.Rate(), 0.01, "Rate")
}

func TestNewRateWindow_NormalizesNonPositiveCap(t *testing.T) {
	w := newRateWindow(0)
	// Should not panic on Add.
	w.Add(1, 1*time.Second)
	require.GreaterOrEqual(t, w.Samples(), 1, "Samples after Add on zero-cap window")
}
