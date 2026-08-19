package eval

import (
	"math"
	"slices"
	"time"
)

// RunConfig records exactly what produced a set of numbers: which embedding
// model and index settings were in force, and how big the index was.
//
// A retrieval score is meaningless on its own — "nDCG@10 = 0.22" says nothing
// unless you also know the embedding model, its dimension, the fusion
// parameters and the size of the haystack. Emitting this alongside every run
// makes results comparable across machines and across time, and makes it
// impossible to read a number without knowing its provenance.
type RunConfig struct {
	// Inputs. How a topic is phrased is an experimental variable, not a
	// constant: FTS5 uses AND semantics, so a verbose natural-language topic
	// matches almost nothing while its keyword reduction scores well, and the
	// dense side can move the other way. A run is only comparable to another
	// run over the same topic file, so record which one was used.
	QrelsPath  string `json:"qrels_path,omitempty"`
	TopicsPath string `json:"topics_path,omitempty"`

	// Corpus
	Messages      int64 `json:"messages"`
	Conversations int64 `json:"conversations"`

	// Embeddings / vector index (zero-valued when only --modes fts is run)
	VectorEnabled  bool    `json:"vector_enabled"`
	EmbeddingModel string  `json:"embedding_model,omitempty"`
	Dimension      int     `json:"embedding_dimension,omitempty"`
	Endpoint       string  `json:"embedding_endpoint,omitempty"`
	Backend        string  `json:"vector_backend,omitempty"`
	Fingerprint    string  `json:"generation_fingerprint,omitempty"`
	RRFK           int     `json:"rrf_k,omitempty"`
	KPerSignal     int     `json:"k_per_signal,omitempty"`
	SubjectBoost   float64 `json:"subject_boost,omitempty"`
	IndexedVectors int64   `json:"indexed_vectors,omitempty"`
	IndexSizeBytes int64   `json:"index_size_bytes,omitempty"`
	IndexPath      string  `json:"index_path,omitempty"`
}

// Latency summarises per-query wall-clock cost for one search mode. Quality
// improvements routinely cost latency or index size; reporting them beside
// the quality metrics stops an "improvement" from hiding an operational
// regression.
type Latency struct {
	Queries  int     `json:"queries"`
	MedianMS float64 `json:"median_ms"`
	P95MS    float64 `json:"p95_ms"`
	TotalMS  float64 `json:"total_ms"`
}

// LatencyTracker accumulates per-query durations for a single mode.
type LatencyTracker struct {
	samples []time.Duration
}

// Add records one query's wall-clock duration.
func (l *LatencyTracker) Add(d time.Duration) { l.samples = append(l.samples, d) }

// Summary reports median, p95 and total over the recorded samples. The p95
// uses nearest-rank on the sorted samples, which is well-defined for the
// small sample counts an eval run produces (it degenerates to "the slowest
// query" below 20 samples — honest, rather than interpolating a percentile
// the data cannot support).
func (l *LatencyTracker) Summary() Latency {
	n := len(l.samples)
	if n == 0 {
		return Latency{}
	}
	sorted := make([]time.Duration, n)
	copy(sorted, l.samples)
	slices.Sort(sorted)

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

	var median float64
	if n%2 == 1 {
		median = ms(sorted[n/2])
	} else {
		median = (ms(sorted[n/2-1]) + ms(sorted[n/2])) / 2
	}

	// Nearest-rank p95: ceil(0.95*n), clamped into range.
	rank := min(max(int(math.Ceil(0.95*float64(n)))-1, 0), n-1)

	var total time.Duration
	for _, d := range l.samples {
		total += d
	}
	return Latency{Queries: n, MedianMS: median, P95MS: ms(sorted[rank]), TotalMS: ms(total)}
}
