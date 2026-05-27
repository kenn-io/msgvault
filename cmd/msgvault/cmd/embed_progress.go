package cmd

import "time"

// rateWindow accumulates per-batch (msgs, elapsed) samples in a ring
// buffer and reports the message-weighted rate over the window:
//
//	rate = sum(msgs) / sum(elapsed_seconds)
//
// Weighting matters when batch sizes vary a lot, e.g. when the worker
// downshifts to BatchSize=1 to drain a poison message: a simple mean
// of per-batch rates would let those tiny batches dominate the
// displayed throughput. Summing first keeps small batches'
// contribution proportional to their size.
type rateWindow struct {
	msgs    []int
	elapsed []time.Duration
	head    int // index of the next write
	count   int // number of valid entries (<= len(msgs))
}

// newRateWindow constructs a ring buffer of the given capacity. A
// non-positive cap is normalized to 1 so callers can always Add and
// Rate without checking — this matches how applyDefaults treats
// non-positive ETAWindow values from TOML, but defends against any
// caller that bypasses defaults.
func newRateWindow(capacity int) *rateWindow {
	if capacity < 1 {
		capacity = 1
	}
	return &rateWindow{
		msgs:    make([]int, capacity),
		elapsed: make([]time.Duration, capacity),
	}
}

// Add records one batch sample. Samples with non-positive elapsed are
// silently skipped: they would either divide by zero (elapsed == 0)
// or contribute negative time (elapsed < 0, only reachable via clock
// non-monotonicity in test harnesses). The next legitimate sample
// re-establishes a healthy window.
func (w *rateWindow) Add(msgs int, elapsed time.Duration) {
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

// Samples returns the number of valid entries currently in the
// window. Useful for the printer's "(last K)" annotation when the
// run hasn't yet emitted enough batches to fill the window.
func (w *rateWindow) Samples() int { return w.count }

// Rate returns the message-weighted rate in messages per second.
// Returns 0 when the window is empty so callers can treat 0 as "no
// estimate yet" and fall back to a no-ETA display branch.
func (w *rateWindow) Rate() float64 {
	if w.count == 0 {
		return 0
	}
	var totalMsgs int
	var totalElapsed time.Duration
	for i := range w.count {
		totalMsgs += w.msgs[i]
		totalElapsed += w.elapsed[i]
	}
	if totalElapsed <= 0 {
		return 0
	}
	return float64(totalMsgs) / totalElapsed.Seconds()
}
