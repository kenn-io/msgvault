//go:build fts5 && sqlite_vec

package cmd

import (
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
)

// evalTestMode is the single search mode these end-to-end runs score. It is
// named once so the flag the run is configured with and the report key its
// assertions read can never drift apart.
const evalTestMode = "fts"

// configureEvalRun points the eval command at one scratch directory: its
// archive as the configured data dir, and the given qrels and topics content
// written into it, for a message-keyed JSON run over the fts mode alone. The
// command's package-level config and flag variables are snapshotted and put
// back when the test ends, so a test that drives runEval directly cannot leak
// its settings into whatever runs next.
func configureEvalRun(t *testing.T, dir, qrels, topics string) {
	t.Helper()

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = dir

	savedQrels, savedTopics, savedModes := evalQrels, evalTopics, evalModes
	savedDocKey, savedLimit, savedJSON := evalDocKey, evalLimit, evalJSON
	t.Cleanup(func() {
		evalQrels, evalTopics, evalModes = savedQrels, savedTopics, savedModes
		evalDocKey, evalLimit, evalJSON = savedDocKey, savedLimit, savedJSON
	})

	evalQrels = writeEvalFile(t, dir, "qrels.txt", qrels)
	evalTopics = writeEvalFile(t, dir, "topics.tsv", topics)
	evalModes = evalTestMode
	evalDocKey = "message"
	evalLimit = 10
	evalJSON = true
}

// TestRunEval_ScoresATopicJudgedEntirelyNonRelevant is the regression for
// conflating "this topic was never judged" with "this topic was judged and
// nothing was relevant".
//
// Both produce an empty relevant set, and the command used to skip on exactly
// that. But an all-non-relevant topic is a real measurement — the run looked,
// and found nothing it should have found — and it can only ever score zero.
// Dropping it therefore removes a zero from every macro average and reports a
// better run than happened. TREC semantics are to score it; only a qid the
// qrels file never mentions has nothing to score against.
//
//   - q1 is judged relevant on the BM25 top hit: MRR 1.0, P@10 0.1.
//   - q2 is judged, every grade 0: MRR 0, P@10 0 — and must be counted.
//   - q3 appears in the topics file only, and must still be skipped.
//
// Before the fix this reported one topic at MRR 1.0; the honest answer is two
// topics at 0.5.
func TestRunEval_ScoresATopicJudgedEntirelyNonRelevant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir,
		"q1 0 <m1@example.com> 1\n"+
			"q2 0 <m1@example.com> 0\n"+
			"q2 0 <m2@example.com> 0\n",
		"q1\trenewal\nq2\trenewal\nq3\trenewal\n")

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	done := captureStdout(t)
	err := runEval(cmd, nil)
	out := done()
	require.NoError(err, "eval run")

	var report struct {
		TopicsEvaluated int `json:"topics_evaluated"`
		Results         map[string]struct {
			MRR    float64 `json:"MRR@10"`
			P      float64 `json:"P@10"`
			Topics int     `json:"topics"`
		} `json:"results"`
	}
	require.NoError(json.Unmarshal([]byte(out), &report), "parse report: %s", out)
	scored := report.Results[evalTestMode]

	assert.Equal(2, report.TopicsEvaluated,
		"the all-non-relevant topic is scored; only the unjudged q3 is skipped")
	assert.Equal(2, scored.Topics)
	assert.InDelta(0.5, scored.MRR, 1e-9,
		"q2 contributes a real zero to the macro average; skipping it reported 1.0")
	assert.InDelta(0.05, scored.P, 1e-9,
		"same for precision: (0.1 + 0) / 2, not 0.1")
}

// TestRunEval_FailsWhenNoTopicIsJudged pins the other side of the split: when
// the qrels file mentions none of the topics there is genuinely nothing to
// score, and the run must still fail with the id-mismatch guidance rather than
// report an empty run full of zeroes.
func TestRunEval_FailsWhenNoTopicIsJudged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir, "other-1 0 <m1@example.com> 0\n", "q1\trenewal\n")

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	err := runEval(cmd, nil)
	require.Error(err, "no topic was judged, so there is nothing to report")
	assert.Contains(err.Error(), "relevance judgments")
}
