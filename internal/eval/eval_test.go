package eval

import (
	"fmt"
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
	assert := assert.New(t)
	ranked := []string{"a", "b", "c", "d"}
	rel := set("a", "c", "x")

	assert.InDelta(1.0, PrecisionAt(ranked, rel, 1), 1e-9)
	assert.InDelta(0.5, PrecisionAt(ranked, rel, 2), 1e-9)
	assert.InDelta(0.5, PrecisionAt(ranked, rel, 4), 1e-9)

	assert.InDelta(1.0/3.0, RecallAt(ranked, rel, 1), 1e-9)
	assert.InDelta(2.0/3.0, RecallAt(ranked, rel, 4), 1e-9)

	// MRR: first relevant ("a") is at rank 1.
	assert.InDelta(1.0, ReciprocalRank(ranked, rel), 1e-9)

	// AP = (1/|rel|) * (P@1 + P@3) = (1/3) * (1/1 + 2/3) = 0.555...
	assert.InDelta((1.0+2.0/3.0)/3.0, AveragePrecision(ranked, rel), 1e-9)

	// nDCG@4: DCG = 1/log2(2) + 1/log2(4) = 1 + 0.5 = 1.5
	//         IDCG (3 relevant) = 1/log2(2)+1/log2(3)+1/log2(4) = 2.13092975
	assert.InDelta(1.5/2.1309297535714578, NDCGAt(ranked, rel, 4), 1e-9)
}

func TestMetrics_EdgeCases(t *testing.T) {
	assert := assert.New(t)
	rel := set("a")
	// No relevant docs at all -> every metric is 0.
	empty := map[string]struct{}{}
	assert.Zero(PrecisionAt([]string{"a"}, empty, 10))
	assert.Zero(RecallAt([]string{"a"}, empty, 10))
	assert.Zero(NDCGAt([]string{"a"}, empty, 10))
	assert.Zero(AveragePrecision([]string{"a"}, empty))
	assert.Zero(ReciprocalRank([]string{"a"}, empty))

	// Empty ranking -> 0.
	assert.Zero(PrecisionAt(nil, rel, 10))
	assert.Zero(RecallAt(nil, rel, 10))
	assert.Zero(ReciprocalRank(nil, rel))

	// Perfect ranking -> P@1 = 1, MRR = 1, nDCG@1 = 1.
	assert.InDelta(1.0, PrecisionAt([]string{"a"}, rel, 1), 1e-9)
	assert.InDelta(1.0, NDCGAt([]string{"a"}, rel, 1), 1e-9)
}

func TestEvaluateAndAggregate(t *testing.T) {
	assert := assert.New(t)
	a := &Aggregate{}
	a.Add(Evaluate([]string{"a", "b"}, set("a"), StandardCutoffs)) // P@10=0.1, MRR=1
	a.Add(Evaluate([]string{"b", "a"}, set("a"), StandardCutoffs)) // MRR=0.5
	assert.Equal(2, a.N)
	mean := a.Mean()
	assert.InDelta((1.0+0.5)/2.0, mean.MRR, 1e-9)
	assert.InDelta(0.1, mean.P, 1e-9) // each had exactly 1 hit in top 10
	// Zero-N aggregate is safe.
	assert.Equal(Scores{}, (&Aggregate{}).Mean())
}

func TestLoadQrels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "qrels.txt")
	// qid iter docid rel  — iter ignored; a malformed line is skipped.
	content := "301 0 docA 1\n301 0 docB 0\n302 0 docC 1\nmalformed line\n301 0 docD 1\n"
	require.NoError(os.WriteFile(p, []byte(content), 0o644))

	q, stats, err := LoadQrels(p)
	require.NoError(err)
	assert.Equal(1, q["301"]["docA"])
	assert.Equal(0, q["301"]["docB"])

	rel := q.RelevantSet("301")
	assert.Equal(set("docA", "docD"), rel) // docB (grade 0) excluded
	assert.Equal(set("docC"), q.RelevantSet("302"))
	assert.Empty(q.RelevantSet("999")) // unknown qid -> empty set

	// The skipped line is counted, not silently swallowed.
	assert.Equal(LoadStats{Path: p, Lines: 5, Parsed: 4, Skipped: 1}, stats)
	assert.False(stats.Suspect(), "one stray line in a good file is not suspicious")
}

// TestLoadQrels_WrongColumnCount is the diagnosability regression. A qrels file
// in the common three-column variant (no iteration column) used to load as an
// empty-but-valid Qrels with no signal at all, and the only downstream symptom
// — "no topics had relevance judgments" — reads like an id mismatch. The counts
// have to make the format problem visible.
func TestLoadQrels_WrongColumnCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	p := filepath.Join(t.TempDir(), "qrels.txt")
	require.NoError(os.WriteFile(p, []byte("301 docA 1\n301 docB 1\n302 docC 1\n"), 0o644))

	q, stats, err := LoadQrels(p)
	require.NoError(err, "a format mismatch is reported through the stats, not as a read error")
	assert.Empty(q)
	assert.Equal(3, stats.Lines)
	assert.Equal(0, stats.Parsed)
	assert.Equal(3, stats.Skipped)
	assert.True(stats.Suspect(), "nothing parsed: the caller must be able to say so")
	assert.Equal("3 lines, 0 parsed, 3 skipped", stats.String())
}

// TestLoadStats_Suspect pins when a caller should warn: nothing usable came
// out, or the skipped lines outnumber the parsed ones. A clean file, or one
// with a stray line, must not trip it.
func TestLoadStats_Suspect(t *testing.T) {
	assert := assert.New(t)
	assert.False(LoadStats{}.Suspect(), "an empty file is a different complaint")
	assert.False(LoadStats{Lines: 10, Parsed: 10}.Suspect())
	assert.False(LoadStats{Lines: 10, Parsed: 9, Skipped: 1}.Suspect())
	assert.True(LoadStats{Lines: 10, Parsed: 4, Skipped: 6}.Suspect())
	assert.True(LoadStats{Lines: 3, Skipped: 3}.Suspect())
}

func TestLoadTopics(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "topics.tsv")
	content := "301\toil and gas drilling\n\n302\tspill response\nno_tab_line\n"
	require.NoError(os.WriteFile(p, []byte(content), 0o644))

	topics, stats, err := LoadTopics(p)
	require.NoError(err)
	require.Len(topics, 2)
	assert.Equal("301", topics[0].ID)
	assert.Equal("oil and gas drilling", topics[0].Query)
	assert.Empty(topics[0].Category, "two-column format has no category")
	assert.Equal("302", topics[1].ID)
	// The blank line is not counted at all; the tabless line is a skip.
	assert.Equal(LoadStats{Path: p, Lines: 3, Parsed: 2, Skipped: 1}, stats)
}

// TestLoadTopics_SpaceSeparated covers the topics-side format mismatch: a file
// written with spaces instead of tabs parses to zero topics, and the counts are
// the only way to tell that apart from an empty file.
func TestLoadTopics_SpaceSeparated(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	p := filepath.Join(t.TempDir(), "topics.tsv")
	require.NoError(os.WriteFile(p, []byte("301 oil and gas\n302 spill response\n"), 0o644))

	topics, stats, err := LoadTopics(p)
	require.NoError(err)
	assert.Empty(topics)
	assert.Equal(LoadStats{Path: p, Lines: 2, Parsed: 0, Skipped: 2}, stats)
	assert.True(stats.Suspect())
}

// TestLoadTopics_CategoryColumn covers the optional third column. A file may
// mix labeled and unlabeled lines; old two-column files must load unchanged.
func TestLoadTopics_CategoryColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "topics.tsv")
	content := "q1\tlease renewal terms\tpointed\n" + // labeled
		"q2\thow did the negotiation conclude\tspanning\n" + // labeled
		"q3\tforklift servicing\n" + // plain two-column line in the same file
		"q4\tinsurance quote\t\n" + // trailing tab, empty category
		"q5\t  padded query  \t  padded  \n" // whitespace trimmed
	require.NoError(os.WriteFile(p, []byte(content), 0o644))

	topics, stats, err := LoadTopics(p)
	require.NoError(err)
	require.Len(topics, 5)
	assert.Equal(Topic{ID: "q1", Query: "lease renewal terms", Category: "pointed"}, topics[0])
	assert.Equal("spanning", topics[1].Category)
	assert.Empty(topics[2].Category, "unlabeled line stays unlabeled")
	assert.Empty(topics[3].Category, "a trailing tab is not a category")
	assert.Equal(Topic{ID: "q5", Query: "padded query", Category: "padded"}, topics[4])
	assert.Equal(LoadStats{Path: p, Lines: 5, Parsed: 5, Skipped: 0}, stats)
}

// TestCutoffsForDepth pins the anti-mislabelling rule: a run that only ever
// looks 20 deep has no recall@100, so the depth it reports must be the depth it
// used. Above the standard depths nothing is clamped.
func TestCutoffsForDepth(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(StandardCutoffs, CutoffsForDepth(100))
	assert.Equal(StandardCutoffs, CutoffsForDepth(1000))
	assert.Equal(Cutoffs{P: 10, NDCG: 10, Recall: 20}, CutoffsForDepth(20))
	assert.Equal(Cutoffs{P: 5, NDCG: 5, Recall: 5}, CutoffsForDepth(5))
	assert.Equal(Cutoffs{P: 1, NDCG: 1, Recall: 1}, CutoffsForDepth(1))
	// A non-positive depth is not a depth; the CLI rejects it before we get
	// here, so fall back to the standard set rather than inventing a zero one.
	assert.Equal(StandardCutoffs, CutoffsForDepth(0))
	assert.Equal(StandardCutoffs, CutoffsForDepth(-1))
}

// TestEvaluate_HonoursCutoffs shows why the clamp matters: with 20 results and
// 40 relevant documents, "recall@100" and recall@20 are different numbers, and
// only one of them is a thing this run measured.
func TestEvaluate_HonoursCutoffs(t *testing.T) {
	assert := assert.New(t)
	rel := map[string]struct{}{}
	var ranked []string
	for i := range 40 {
		id := fmt.Sprintf("d%02d", i)
		rel[id] = struct{}{}
		if i < 20 {
			ranked = append(ranked, id)
		}
	}
	atStandard := Evaluate(ranked, rel, StandardCutoffs)
	atDepth := Evaluate(ranked, rel, CutoffsForDepth(20))

	assert.InDelta(0.5, atStandard.Recall, 1e-9, "R@100 over a 20-deep list is bounded by the depth")
	assert.InDelta(0.5, atDepth.Recall, 1e-9)
	// P and nDCG are unaffected here because 10 <= 20; the point is that the
	// reported label, not the value, is what changes.
	assert.InDelta(atStandard.P, atDepth.P, 1e-9)
	assert.InDelta(atStandard.NDCG, atDepth.NDCG, 1e-9)

	// Below the precision depth the clamp does change the value: 3 retrieved,
	// all relevant, is precision 1.0 at depth 3 and 0.3 at depth 10.
	short := []string{"d00", "d01", "d02"}
	assert.InDelta(0.3, Evaluate(short, rel, StandardCutoffs).P, 1e-9)
	assert.InDelta(1.0, Evaluate(short, rel, CutoffsForDepth(3)).P, 1e-9)
}

func TestLoadQrels_MissingFile(t *testing.T) {
	_, _, err := LoadQrels(filepath.Join(t.TempDir(), "nope.txt"))
	require.Error(t, err)
}
