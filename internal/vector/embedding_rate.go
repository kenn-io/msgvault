package vector

import "time"

// EmbeddingRateWindow retains completed batch coverage and elapsed durations.
type EmbeddingRateWindow struct {
	msgs    []int
	elapsed []time.Duration
	head    int // index of the next write
	count   int // number of valid entries (<= len(msgs))
}

// NewEmbeddingRateWindow retains the configured number of completed batches.
func NewEmbeddingRateWindow(capacity int) *EmbeddingRateWindow {
	if capacity < 1 {
		capacity = 1
	}
	return &EmbeddingRateWindow{
		msgs:    make([]int, capacity),
		elapsed: make([]time.Duration, capacity),
	}
}

// Add records a completed batch with a positive elapsed duration.
func (w *EmbeddingRateWindow) Add(msgs int, elapsed time.Duration) {
	if elapsed <= 0 {
		return
	}
	w.msgs[w.head] = msgs
	w.elapsed[w.head] = elapsed
	w.head = (w.head + 1) % len(w.msgs)
	if w.count < len(w.msgs) {
		w.count++
	}
}

// Samples returns the number of retained completed batches.
func (w *EmbeddingRateWindow) Samples() int { return w.count }

// Rate returns committed messages per second over completed batch durations.
func (w *EmbeddingRateWindow) Rate() float64 {
	if w.count == 0 {
		return 0
	}
	var totalMsgs int
	for i := range w.count {
		totalMsgs += w.msgs[i]
	}
	return float64(max(0, totalMsgs)) / w.Elapsed().Seconds()
}

// Elapsed returns the summed durations of the retained completed batches.
func (w *EmbeddingRateWindow) Elapsed() time.Duration {
	var elapsed time.Duration
	for i := range w.count {
		elapsed += w.elapsed[i]
	}
	return elapsed
}
