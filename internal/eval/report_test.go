package eval

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatencyTracker_Summary(t *testing.T) {
	var l LatencyTracker
	// Odd count -> median is the middle sample.
	for _, ms := range []int{30, 10, 20} {
		l.Add(time.Duration(ms) * time.Millisecond)
	}
	s := l.Summary()
	assert.Equal(t, 3, s.Queries)
	assert.InDelta(t, 20.0, s.MedianMS, 1e-6)
	assert.InDelta(t, 60.0, s.TotalMS, 1e-6)
	// Nearest-rank p95 on 3 samples is the slowest one.
	assert.InDelta(t, 30.0, s.P95MS, 1e-6)
}

func TestLatencyTracker_EvenCountMedian(t *testing.T) {
	var l LatencyTracker
	for _, ms := range []int{40, 10, 30, 20} {
		l.Add(time.Duration(ms) * time.Millisecond)
	}
	s := l.Summary()
	assert.Equal(t, 4, s.Queries)
	assert.InDelta(t, 25.0, s.MedianMS, 1e-6) // (20+30)/2
	assert.InDelta(t, 100.0, s.TotalMS, 1e-6)
	assert.InDelta(t, 40.0, s.P95MS, 1e-6)
}

func TestLatencyTracker_Empty(t *testing.T) {
	var l LatencyTracker
	assert.Equal(t, Latency{}, l.Summary())
}

func TestLatencyTracker_P95NearestRank(t *testing.T) {
	var l LatencyTracker
	// 100 samples, 1..100 ms: nearest-rank p95 is the 95th slowest.
	for i := 1; i <= 100; i++ {
		l.Add(time.Duration(i) * time.Millisecond)
	}
	s := l.Summary()
	assert.InDelta(t, 95.0, s.P95MS, 1e-6)
	assert.InDelta(t, 50.5, s.MedianMS, 1e-6)
}

// A result must never be readable without knowing what produced it, so the
// provenance block has to survive serialisation.
func TestRunConfig_JSONCarriesProvenance(t *testing.T) {
	p := RunConfig{
		Messages: 18401, Conversations: 18401,
		VectorEnabled: true, EmbeddingModel: "bge-m3", Dimension: 1024,
		Endpoint: "http://127.0.0.1:8123/v1", Backend: "sqlite-vec",
		Fingerprint: "abc123", RRFK: 60, KPerSignal: 100, SubjectBoost: 1.25,
		IndexedVectors: 18401, IndexSizeBytes: 81_500_000, IndexPath: "/tmp/vectors.db",
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)

	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	for _, k := range []string{
		"messages", "conversations", "embedding_model", "embedding_dimension",
		"vector_backend", "generation_fingerprint", "rrf_k", "k_per_signal",
		"subject_boost", "indexed_vectors", "index_size_bytes",
	} {
		assert.Contains(t, back, k, "provenance field %q must be serialised", k)
	}
	assert.Equal(t, "bge-m3", back["embedding_model"])
}

// An fts-only run has no vector config; the block must degrade cleanly rather
// than reporting a zero-dimension model that never ran.
func TestRunConfig_FTSOnlyOmitsVectorFields(t *testing.T) {
	raw, err := json.Marshal(RunConfig{Messages: 10, Conversations: 4})
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, false, back["vector_enabled"])
	assert.NotContains(t, back, "embedding_model")
	assert.NotContains(t, back, "index_size_bytes")
	assert.Contains(t, back, "messages")
}
