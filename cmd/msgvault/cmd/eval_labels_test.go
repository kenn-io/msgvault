//go:build fts5 && sqlite_vec

package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunEval_LabelsMAPAndMRRAtTheRetrievalDepth pins the labels against the
// depth the run actually looked to.
//
// MAP and MRR take no cutoff, so they were reported bare — but the ranking they
// score is truncated to -n before they ever see it, which makes a relevant
// message below that rank invisible to them exactly as it is to recall. A run
// at -n 5 reports MAP@5 and MRR@5; calling them "MAP" and "MRR" offers them for
// comparison against a run that retrieved a hundred deep, which is the same
// mislabeling the clamped P/nDCG/R headers already exist to prevent.
func TestRunEval_LabelsMAPAndMRRAtTheRetrievalDepth(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir, "q1 0 <m1@example.com> 1\n", "q1\trenewal\n")
	evalLimit = 5

	var report struct {
		Cutoffs map[string]int            `json:"cutoffs"`
		Results map[string]map[string]any `json:"results"`
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	done := captureStdout(t)
	err := runEval(cmd, nil)
	out := done()
	require.NoError(err, "eval run")
	require.NoError(json.Unmarshal([]byte(out), &report), "parse report: %s", out)

	scored := report.Results[evalTestMode]
	assert.Contains(scored, "MAP@5", "the JSON metric key names the depth; %v", scored)
	assert.Contains(scored, "MRR@5")
	assert.NotContains(scored, "MAP", "an unqualified key would claim an untruncated ranking")
	assert.NotContains(scored, "MRR")
	// Every metric's depth is readable the same way, including the two whose
	// depth is the retrieval depth rather than a cutoff of their own.
	assert.Equal(map[string]int{"precision": 5, "ndcg": 5, "recall": 5, "map": 5, "mrr": 5},
		report.Cutoffs)
}

// TestEvalReport_TableLabelsMAPAndMRRAtTheRetrievalDepth is the same rule for
// the human-readable output, which builds its header row separately from the
// JSON keys.
func TestEvalReport_TableLabelsMAPAndMRRAtTheRetrievalDepth(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	configureEvalRun(t, dir, "q1 0 <m1@example.com> 1\n", "q1\trenewal\n")
	evalLimit = 5
	evalJSON = false

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	done := captureStdout(t)
	err := runEval(cmd, nil)
	out := done()
	require.NoError(err, "eval run")

	header := ""
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "MODE") {
			header = line
			break
		}
	}
	require.NotEmpty(header, "no results table in:\n%s", out)
	assert.Contains(header, "MAP@5")
	assert.Contains(header, "MRR@5")
}
