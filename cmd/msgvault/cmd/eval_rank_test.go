//go:build sqlite_vec

package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

// threadedCorpus builds a ranked corpus of threads*perThread messages, ordered
// thread by thread: the first perThread hits all belong to one conversation.
// That is the shape that breaks a truncate-then-collapse ranking.
func threadedCorpus(threads, perThread int) []query.MessageSummary {
	out := make([]query.MessageSummary, 0, threads*perThread)
	var id int64
	for t := range threads {
		for m := range perThread {
			id++
			out = append(out, query.MessageSummary{
				ID:                   id,
				SourceMessageID:      fmt.Sprintf("<t%03d-m%d@example.com>", t, m),
				SourceConversationID: fmt.Sprintf("thread-%03d", t),
			})
		}
	}
	return out
}

// pagingEngine serves the first n results of corpus and records the depths it
// was asked for, so a test can assert both the answer and the work done to get
// it.
type pagingEngine struct {
	*querytest.MockEngine

	depths []int
}

func newPagingEngine(corpus []query.MessageSummary) *pagingEngine {
	e := &pagingEngine{MockEngine: &querytest.MockEngine{}}
	e.SearchFunc = func(_ context.Context, _ *search.Query, limit, _ int) ([]query.MessageSummary, error) {
		e.depths = append(e.depths, limit)
		if limit > len(corpus) {
			limit = len(corpus)
		}
		return corpus[:limit], nil
	}
	return e
}

// evalTestLimit is the retrieval depth every ranking test uses; it matches the
// command's own -n default, so the over-fetch arithmetic in the assertions is
// the arithmetic a real run does.
const evalTestLimit = 100

func newTestEvaluator(t *testing.T, eng query.Engine, docKey string) (*evaluator, *runDiagnostics) {
	t.Helper()
	spec, ok := newDocKeyRegistry()[docKey]
	require.True(t, ok)
	diag := &runDiagnostics{}
	return &evaluator{ctx: t.Context(), qeng: eng, key: spec, limit: evalTestLimit, diag: diag}, diag
}

// TestRankedFTS_ConversationKeyFillsTheRequestedDepth is the regression for the
// ordering bug. The corpus has 4 messages per thread, so a raw fetch of 100
// covers only 25 threads. Collapsing after truncating returned those 25 and
// reported them as "R@100"; the ranking must instead over-fetch, collapse, and
// hand back 100 distinct threads.
func TestRankedFTS_ConversationKeyFillsTheRequestedDepth(t *testing.T) {
	corpus := threadedCorpus(200, 4)
	eng := newPagingEngine(corpus)
	ev, diag := newTestEvaluator(t, eng, "conversation")

	// For contrast, what the old ordering produced: collapsing the first
	// --limit raw hits only ever reached a quarter of the requested depth.
	firstPage := make([]string, 0, 100)
	for _, m := range corpus[:100] {
		firstPage = append(firstPage, m.SourceConversationID)
	}
	require.Len(t, eval.DedupeKeys(firstPage), 25, "truncate-then-collapse caps out at 25 threads here")

	ranked, err := ev.rankedFTS("lease renewal")
	require.NoError(t, err)

	require.Len(t, ranked, 100, "-n 100 with --doc-key=conversation means 100 distinct threads")
	assert.Equal(t, "thread-000", ranked[0], "best rank wins")
	assert.Equal(t, "thread-099", ranked[99])
	seen := map[string]struct{}{}
	for _, k := range ranked {
		_, dup := seen[k]
		require.False(t, dup, "collapsed ranking must not repeat a thread: %s", k)
		seen[k] = struct{}{}
	}
	assert.Equal(t, []int{400}, eng.depths, "one over-fetch was enough here")
	assert.Zero(t, diag.DepthShortfalls)
}

// TestRankedFTS_MessageKeyDoesNotOverFetch: the message doc-key is 1:1 with
// hits, so padding the query would only inflate the latency this command
// reports.
func TestRankedFTS_MessageKeyDoesNotOverFetch(t *testing.T) {
	eng := newPagingEngine(threadedCorpus(200, 5))
	ev, _ := newTestEvaluator(t, eng, "message")

	ranked, err := ev.rankedFTS("lease renewal")
	require.NoError(t, err)
	assert.Len(t, ranked, 100)
	assert.Equal(t, []int{100}, eng.depths, "no over-fetch for a 1:1 doc-key")
}

// TestRankedFTS_GrowsThePoolUntilTheDepthIsFilled: one over-fetch is not always
// enough. With 50 messages per thread, limit*4 still only covers 8 threads, so
// the pool has to grow.
func TestRankedFTS_GrowsThePoolUntilTheDepthIsFilled(t *testing.T) {
	eng := newPagingEngine(threadedCorpus(200, 50))
	ev, diag := newTestEvaluator(t, eng, "conversation")

	ranked, err := ev.rankedFTS("lease renewal")
	require.NoError(t, err)
	assert.Len(t, ranked, 100)
	assert.Equal(t, []int{400, 1600, 6400}, eng.depths, "the pool grows geometrically")
	assert.Zero(t, diag.DepthShortfalls, "the depth was filled on the last attempt")
}

// TestRankedFTS_StopsWhenTheEngineIsExhausted: a corpus smaller than the pool
// must not trigger pointless retries, and the short answer is not a shortfall
// worth warning about — there is simply nothing more to retrieve.
func TestRankedFTS_StopsWhenTheEngineIsExhausted(t *testing.T) {
	eng := newPagingEngine(threadedCorpus(6, 5)) // 30 messages, 6 threads
	ev, diag := newTestEvaluator(t, eng, "conversation")

	ranked, err := ev.rankedFTS("lease renewal")
	require.NoError(t, err)
	assert.Len(t, ranked, 6, "six threads exist; six threads come back")
	assert.Equal(t, []int{400}, eng.depths, "the engine came back short, so stop")
	assert.Zero(t, diag.DepthShortfalls)
}

// TestRankedFTS_ReportsADepthShortfall: when even the largest pool cannot fill
// the depth, the metrics are computed over a shallower list than requested and
// the run has to say so rather than passing the number off as full depth.
func TestRankedFTS_ReportsADepthShortfall(t *testing.T) {
	// One single thread, deeper than the biggest pool: every fetch is
	// saturated, and every fetch collapses to one key.
	eng := newPagingEngine(threadedCorpus(1, 20_000))
	ev, diag := newTestEvaluator(t, eng, "conversation")

	ranked, err := ev.rankedFTS("lease renewal")
	require.NoError(t, err)
	assert.Equal(t, []string{"thread-000"}, ranked)
	assert.Equal(t, []int{400, 1600, 6400}, eng.depths, "the pool grows to the documented ceiling and stops")
	assert.Equal(t, 1, diag.DepthShortfalls)
	assert.Contains(t, diag.notes()[0], "could not fill")
}

// TestRankedVector_FilterOnlyTopicIsRecoverable: a topic that parses to filters
// only has nothing to embed. It must surface as errNoFreeText — which runEval
// turns into a skipped cell — and never as an opaque failure that aborts the
// run and discards every score computed so far.
func TestRankedVector_FilterOnlyTopicIsRecoverable(t *testing.T) {
	const topic = "from:alice@example.com"
	require.Empty(t, search.Parse(topic).TextTerms, "fixture assumption: this topic is filter-only")

	// heng is deliberately nil: the check must happen before the engine is
	// ever touched.
	ev, _ := newTestEvaluator(t, &querytest.MockEngine{}, "message")

	_, err := ev.rankedVector("vector", topic)
	require.Error(t, err)
	require.ErrorIs(t, err, errNoFreeText)
	assert.Contains(t, err.Error(), topic, "the error names the offending topic")
}

// TestRunDiagnostics_Notes checks that every silent failure mode this run can
// hit produces a line a user will actually read.
func TestRunDiagnostics_Notes(t *testing.T) {
	assert.Empty(t, (&runDiagnostics{}).notes(), "a clean run says nothing")

	d := &runDiagnostics{UnhydratedHits: 7}
	require.Len(t, d.notes(), 1)
	assert.Contains(t, d.notes()[0], "7 retrieved hits could not be hydrated")

	d = &runDiagnostics{}
	d.skip("q3", "hybrid", "no free-text terms to embed (filter-only topic)")
	require.Len(t, d.notes(), 1)
	assert.Contains(t, d.notes()[0], "topic q3 / hybrid")
}
