package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func set(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

// ranked = [a b c d], relevant = {a, c, x}; x is never retrieved.
func TestMetrics_KnownValues(t *testing.T) {
	ranked := []string{"a", "b", "c", "d"}
	rel := set("a", "c", "x")

	assert.InDelta(t, 1.0, PrecisionAt(ranked, rel, 1), 1e-9)
	assert.InDelta(t, 0.5, PrecisionAt(ranked, rel, 2), 1e-9)
	assert.InDelta(t, 0.5, PrecisionAt(ranked, rel, 4), 1e-9)

	assert.InDelta(t, 1.0/3.0, RecallAt(ranked, rel, 1), 1e-9)
	assert.InDelta(t, 2.0/3.0, RecallAt(ranked, rel, 4), 1e-9)

	// MRR: first relevant ("a") is at rank 1.
	assert.InDelta(t, 1.0, ReciprocalRank(ranked, rel), 1e-9)

	// AP = (1/|rel|) * (P@1 + P@3) = (1/3) * (1/1 + 2/3) = 0.555...
	assert.InDelta(t, (1.0+2.0/3.0)/3.0, AveragePrecision(ranked, rel), 1e-9)

	// nDCG@4: DCG = 1/log2(2) + 1/log2(4) = 1 + 0.5 = 1.5
	//         IDCG (3 relevant) = 1/log2(2)+1/log2(3)+1/log2(4) = 2.13092975
	assert.InDelta(t, 1.5/2.1309297535714578, NDCGAt(ranked, rel, 4), 1e-9)
}

func TestMetrics_EdgeCases(t *testing.T) {
	rel := set("a")
	// No relevant docs at all -> every metric is 0.
	empty := map[string]struct{}{}
	assert.Zero(t, PrecisionAt([]string{"a"}, empty, 10))
	assert.Zero(t, RecallAt([]string{"a"}, empty, 10))
	assert.Zero(t, NDCGAt([]string{"a"}, empty, 10))
	assert.Zero(t, AveragePrecision([]string{"a"}, empty))
	assert.Zero(t, ReciprocalRank([]string{"a"}, empty))

	// Empty ranking -> 0.
	assert.Zero(t, PrecisionAt(nil, rel, 10))
	assert.Zero(t, RecallAt(nil, rel, 10))
	assert.Zero(t, ReciprocalRank(nil, rel))

	// Perfect ranking -> P@1 = 1, MRR = 1, nDCG@1 = 1.
	assert.InDelta(t, 1.0, PrecisionAt([]string{"a"}, rel, 1), 1e-9)
	assert.InDelta(t, 1.0, NDCGAt([]string{"a"}, rel, 1), 1e-9)
}

func TestEvaluateAndAggregate(t *testing.T) {
	a := &Aggregate{}
	a.Add(Evaluate([]string{"a", "b"}, set("a")))       // P@10=0.1, MRR=1
	a.Add(Evaluate([]string{"b", "a"}, set("a")))       // MRR=0.5
	assert.Equal(t, 2, a.N)
	mean := a.Mean()
	assert.InDelta(t, (1.0+0.5)/2.0, mean.MRR, 1e-9)
	assert.InDelta(t, 0.1, mean.P10, 1e-9) // each had exactly 1 hit in top 10
	// Zero-N aggregate is safe.
	assert.Equal(t, Scores{}, (&Aggregate{}).Mean())
}

func TestLoadQrels(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "qrels.txt")
	// qid iter docid rel  — iter ignored; a malformed line is skipped.
	content := "301 0 docA 1\n301 0 docB 0\n302 0 docC 1\nmalformed line\n301 0 docD 1\n"
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))

	q, err := LoadQrels(p)
	require.NoError(t, err)
	assert.Equal(t, 1, q["301"]["docA"])
	assert.Equal(t, 0, q["301"]["docB"])

	rel := q.RelevantSet("301")
	assert.Equal(t, set("docA", "docD"), rel) // docB (grade 0) excluded
	assert.Equal(t, set("docC"), q.RelevantSet("302"))
	assert.Empty(t, q.RelevantSet("999")) // unknown qid -> empty set
}

func TestLoadTopics(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "topics.tsv")
	content := "301\toil and gas drilling\n\n302\tspill response\nno_tab_line\n"
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))

	topics, err := LoadTopics(p)
	require.NoError(t, err)
	require.Len(t, topics, 2)
	assert.Equal(t, "301", topics[0].ID)
	assert.Equal(t, "oil and gas drilling", topics[0].Query)
	assert.Equal(t, "302", topics[1].ID)
}

func TestLoadQrels_MissingFile(t *testing.T) {
	_, err := LoadQrels(filepath.Join(t.TempDir(), "nope.txt"))
	require.Error(t, err)
}
