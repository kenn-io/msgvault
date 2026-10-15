package vector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateWindow_Samples(t *testing.T) {
	type sample struct {
		messages int
		elapsed  time.Duration
	}
	for _, tc := range []struct {
		name     string
		capacity int
		samples  []sample
		count    int
		rate     float64
		delta    float64
	}{
		{name: "empty", capacity: 10},
		{name: "partial fill", capacity: 10, samples: []sample{{50, time.Second}, {100, time.Second}}, count: 2, rate: 75},
		{name: "zero elapsed", capacity: 10, samples: []sample{{10, time.Second}, {50, 0}, {20, time.Second}}, count: 2, rate: 15},
		{name: "negative elapsed", capacity: 10, samples: []sample{{10, time.Second}, {50, -time.Second}}, count: 1, rate: 10},
		{name: "eviction", capacity: 3, samples: []sample{{10, time.Second}, {10, time.Second}, {10, time.Second}, {1000, time.Second}}, count: 3, rate: 340},
		{name: "weighted rate", capacity: 2, samples: []sample{{1, 10 * time.Second}, {100, time.Second}}, count: 2, rate: 101.0 / 11.0, delta: 0.01},
		{name: "zero capacity", samples: []sample{{1, time.Second}}, count: 1, rate: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewEmbeddingRateWindow(tc.capacity)
			for _, sample := range tc.samples {
				w.Add(sample.messages, sample.elapsed)
			}
			require.Equal(t, tc.count, w.Samples())
			require.InDelta(t, tc.rate, w.Rate(), tc.delta)
		})
	}
}
