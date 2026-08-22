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
		MRR    float64 `json:"MRR@10"`
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
	note := findEvalNote(t, report.Diagnostics.UnjudgedTopics, report.TopicsEvaluated, 4)
	assert.Contains(note, "3 of 4 topics had no matching qrels entry")
	assert.Contains(note, "q2, q3, q4")
	assert.Contains(note, "cover 1 of the topics file")
}

// TestRunEval_UnjudgedCoverageNoteCountsWhatWasActuallyScored is the
// regression for conflating "judged" with "scored". A judged topic that
// parses to no search criteria is skipped, same as an unjudged one, so
// subtracting only the unjudged count from the topics-file total overstates
// what the headline numbers cover. Here q1 scores, q2 is judged but
// criteria-less, q3 is unjudged: TopicsEvaluated is 1, not
// Parsed-len(unjudged) = 3, and the note has to say 1, matching the same
// number the JSON report's own topics_evaluated is built from.
func TestRunEval_UnjudgedCoverageNoteCountsWhatWasActuallyScored(t *testing.T) {
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir,
		"q1 0 <m1@example.com> 1\n"+
			"q2 0 <m1@example.com> 1\n",
		"q1\trenewal\nq2\tsubject:\"\"\nq3\trenewal\n")

	var report evalTopicReport
	runEvalForReport(t, &report)

	assert.Equal(1, report.TopicsEvaluated, "only q1 scores; q2 is judged but criteria-less")
	assert.Equal([]string{"q3"}, report.Diagnostics.UnjudgedTopics)

	note := findEvalNote(t, report.Diagnostics.UnjudgedTopics, report.TopicsEvaluated, 3)
	assert.Contains(note, "1 of 3 topics had no matching qrels entry")
	assert.Contains(note, "cover 1 of the topics file",
		"not 2 (Parsed-unjudged): q2 was judged but never scored")
}

// TestRunEval_CountsEachTopicOnceForARepeatedMode is the end-to-end regression
// for a duplicated --modes value. The aggregates are keyed by mode name, so
// `--modes fts,fts` added every topic's score to the fts aggregate twice: the
// report claimed two topics for a two-topic run over one mode, over a run that
// had also issued every query twice. The count beside the means is the
// denominator a reader compares two runs by, so it has to be the number of
// topics, not the number of times the list mentioned the mode.
func TestRunEval_CountsEachTopicOnceForARepeatedMode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir,
		"q1 0 <m1@example.com> 1\nq2 0 <m2@example.com> 1\n",
		"q1\trenewal\nq2\trenewal\n")
	// configureEvalRun snapshots and restores every eval flag, so overriding
	// one after it is safe.
	evalModes = evalTestMode + "," + evalTestMode

	var report evalTopicReport
	runEvalForReport(t, &report)

	require.Len(report.Results, 1, "a repeated mode is still one mode")
	assert.Equal(2, report.TopicsEvaluated)
	assert.Equal(2, report.Results[evalTestMode].Topics,
		"two topics scored once each; counting the mode twice reported 4")
}

// findEvalNote re-renders the diagnostics the table output would print and
// returns the line covering the given unjudged topics. scored is the number
// of topics the run actually scored and parsed is the topics file's total,
// both matching runEval's own diag.scored assignment and TopicsLoad.Parsed —
// a caller passing report.TopicsEvaluated and the real topics count keeps
// the synthetic diagnostics consistent with the run it is standing in for.
func findEvalNote(t *testing.T, unjudged []string, scored, parsed int) string {
	t.Helper()
	diag := &runDiagnostics{UnjudgedTopics: unjudged, scored: scored}
	diag.TopicsLoad = eval.LoadStats{Path: "topics.tsv", Lines: parsed, Parsed: parsed}
	for _, n := range diag.notes() {
		if strings.Contains(n, "no matching qrels entry") {
			return n
		}
	}
	t.Fatalf("no coverage note in %v", diag.notes())
	return ""
}
