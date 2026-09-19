//go:build sqlite_vec

package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// parsedEvalModes runs the flag parser and returns the accepted modes as the
// comma-separated list they came in as, so an expectation can be written as one
// string rather than as a slice of bare mode names.
func parsedEvalModes(t *testing.T, spec string) (string, bool) {
	t.Helper()
	modes, needVec, err := parseEvalModes(spec)
	require.NoError(t, err, "parse --modes %q", spec)
	return strings.Join(modes, ","), needVec
}

// TestParseEvalModes_DeduplicatesPreservingOrder is the regression for a
// repeated mode being evaluated twice.
//
// Each entry in the list gets its own pass through the scoring loop but shares
// one Aggregate and one LatencyTracker per mode name, so a duplicate added
// every topic's score to that mode a second time — doubling the topic count the
// report prints beside the means, and doubling the queries actually run. Order
// is the order the report's rows appear in, so the first mention keeps its
// position.
func TestParseEvalModes_DeduplicatesPreservingOrder(t *testing.T) {
	assert := assert.New(t)

	modes, needVec := parsedEvalModes(t, "hybrid,fts,hybrid,fts")
	assert.Equal("hybrid,fts", modes,
		"each mode once, at the position it was first named")
	assert.True(needVec, "the surviving hybrid entry still opens the vector path")
}

// TestParseEvalModes_DedupesASingleRepeatedMode covers the flag as a user is
// most likely to mistype it, and pins that dropping the duplicate does not also
// drop the vector requirement it carried.
func TestParseEvalModes_DedupesASingleRepeatedMode(t *testing.T) {
	assert := assert.New(t)

	modes, needVec := parsedEvalModes(t, " fts , fts ")
	assert.Equal("fts", modes, "surrounding whitespace does not make a second mode")
	assert.False(needVec, "fts alone never opens the vector path")

	modes, needVec = parsedEvalModes(t, "vector,vector")
	assert.Equal("vector", modes)
	assert.True(needVec, "the surviving entry still needs the index")
}

// TestParseEvalModes_StillRejectsARepeatedInvalidMode keeps the dedupe from
// swallowing the validation: the switch has to run on every entry, before the
// duplicate check, or `--modes fts,bogus,bogus` would quietly report on fts
// alone.
func TestParseEvalModes_StillRejectsARepeatedInvalidMode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	modes, _, err := parseEvalModes("fts,bogus,bogus")
	require.Error(err, "an unknown mode is an error however often it appears")
	assert.Empty(modes)
	assert.Contains(err.Error(), `"bogus"`)
}

// TestParseEvalModes_AcceptsTheFullSetAndRejectsAnEmptyOne pins the two ends of
// the flag the dedupe must leave alone: distinct modes all survive, and a list
// naming none of them is still rejected rather than silently scoring nothing.
func TestParseEvalModes_AcceptsTheFullSetAndRejectsAnEmptyOne(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	modes, needVec := parsedEvalModes(t, "fts,vector,hybrid")
	assert.Equal("fts,vector,hybrid", modes, "distinct modes all survive, in order")
	assert.True(needVec)

	_, _, err := parseEvalModes(" , ,")
	require.Error(err, "a list of nothing but separators names no mode")
	assert.Contains(err.Error(), "--modes is empty")
}

// TestRequireFTS5ForModes_RejectsFTSModeWithoutFTS5 is the regression for a
// run that would otherwise silently score a LIKE-and-recency fallback while
// its own report still calls the mode "fts" and implies BM25 ranking.
func TestRequireFTS5ForModes_RejectsFTSModeWithoutFTS5(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	err := requireFTS5ForModes([]string{evalTestMode}, false)
	require.Error(err, "fts without FTS5 must stop the run, not silently degrade it")
	assert.Contains(err.Error(), "FTS5")
	assert.Contains(err.Error(), "--modes fts")
}

// TestRequireFTS5ForModes_AllowsWhatDoesNotNeedFTS5 pins the two ways this
// guard must stay out of the way: fts5 actually being available, and a
// --modes list that never asked for fts in the first place (vector/hybrid
// alone must not be blocked by an FTS5 outage they don't depend on).
func TestRequireFTS5ForModes_AllowsWhatDoesNotNeedFTS5(t *testing.T) {
	require := require.New(t)

	require.NoError(requireFTS5ForModes([]string{evalTestMode, string(hybrid.ModeVector)}, true),
		"fts is fine once FTS5 is actually available")
	require.NoError(requireFTS5ForModes([]string{string(hybrid.ModeVector), string(hybrid.ModeHybrid)}, false),
		"neither mode here reads through Store's FTS path")
}
