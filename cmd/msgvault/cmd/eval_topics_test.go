//go:build fts5 && sqlite_vec

package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/eval"
)

// evalTopicReport is the slice of the JSON report the topic-handling tests
// read: how many topics were scored, what the scored ones came to, and what the
// run said about the ones it did not score.
type evalTopicReport struct {
	TopicsEvaluated int `json:"topics_evaluated"`
	Results         map[string]struct {
		MRR    float64 `json:"MRR"`
		Topics int     `json:"topics"`
	} `json:"results"`
	Diagnostics struct {
		SkippedCells   []string `json:"skipped_cells"`
		UnjudgedTopics []string `json:"unjudged_topics"`
	} `json:"diagnostics"`
}

// runEvalForReport drives the configured eval run and decodes its JSON report.
func runEvalForReport(t *testing.T, out *evalTopicReport) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	done := captureStdout(t)
	err := runEval(cmd, nil)
	text := done()
	require.NoError(t, err, "eval run")
	require.NoError(t, json.Unmarshal([]byte(text), out), "parse report: %s", text)
}

// TestRunEval_SkipsATopicThatParsedToAnEmptyQuery is the regression for a topic
// that is non-empty text, parses without error, and still carries no search
// criteria.
//
// `subject:""` is the plain case: the parser drops an empty operator value
// rather than building a `LIKE '%%'` that matches everything, so Query.Err() is
// nil and Query.IsEmpty() is true. Scoring it does not measure retrieval at
// all — the fts path answers an empty query by listing the whole live corpus,
// newest first — so the topic collects whatever the archive's date
// distribution hands it. Here that is a spurious MRR of 0.5 for q2: its judged
// message is the archive's oldest, so the corpus listing puts it second, and
// the run reported a headline 0.75 over "two" topics.
//
// The honest answer is one topic at MRR 1.0, with q2 reported as unscorable.
func TestRunEval_SkipsATopicThatParsedToAnEmptyQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir,
		"q1 0 <m1@example.com> 1\n"+
			"q2 0 <m1@example.com> 1\n",
		"q1\trenewal\nq2\tsubject:\"\"\n")

	var report evalTopicReport
	runEvalForReport(t, &report)
	scored := report.Results[evalTestMode]

	assert.Equal(1, report.TopicsEvaluated, "the criteria-less topic is not a measurement")
	assert.Equal(1, scored.Topics)
	assert.InDelta(1.0, scored.MRR, 1e-9,
		"only q1 is scored; folding in the full-corpus scan reported 0.75")

	require.Len(report.Diagnostics.SkippedCells, 1, "the skip must be reported, not silent")
	assert.Contains(report.Diagnostics.SkippedCells[0], "topic q2")
	assert.Contains(report.Diagnostics.SkippedCells[0], "no search criteria")
}

// TestRunEval_ReportsTopicsWithNoMatchingJudgments pins the coverage half of
// the qrels join. Not scoring an unjudged topic is right — there is nothing to
// score it against — but one judged topic is enough for the command to print a
// headline number, so a qrels file that matches a fraction of the topics file
// reports a mean over a small, self-selected subset and looks like a complete
// run. Here three of four topics go unjudged and the survivor scores a perfect
// 1.0; the run has to say which three it dropped and out of how many.
func TestRunEval_ReportsTopicsWithNoMatchingJudgments(t *testing.T) {
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir,
		"q1 0 <m1@example.com> 1\n",
		"q1\trenewal\nq2\trenewal\nq3\trenewal\nq4\trenewal\n")

	var report evalTopicReport
	runEvalForReport(t, &report)

	assert.Equal(1, report.TopicsEvaluated)
	assert.Equal([]string{"q2", "q3", "q4"}, report.Diagnostics.UnjudgedTopics,
		"the unscored topics are named, in topics-file order")
	assert.Empty(report.Diagnostics.SkippedCells,
		"an unjudged topic is not a skip: nothing went wrong with it")

	// The same fact has to reach the table output, which renders notes() and
	// never sees the JSON block.
	note := findEvalNote(t, report.Diagnostics.UnjudgedTopics)
	assert.Contains(note, "3 of 4 topics had no matching qrels entry")
	assert.Contains(note, "q2, q3, q4")
}

// findEvalNote re-renders the diagnostics the table output would print and
// returns the line covering the given unjudged topics.
func findEvalNote(t *testing.T, unjudged []string) string {
	t.Helper()
	diag := &runDiagnostics{UnjudgedTopics: unjudged}
	diag.TopicsLoad = eval.LoadStats{Path: "topics.tsv", Lines: 4, Parsed: 4}
	for _, n := range diag.notes() {
		if strings.Contains(n, "no matching qrels entry") {
			return n
		}
	}
	t.Fatalf("no coverage note in %v", diag.notes())
	return ""
}
