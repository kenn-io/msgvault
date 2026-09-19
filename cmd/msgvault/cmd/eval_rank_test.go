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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// vectorTestGeneration is the active generation the fake backend serves; the
// hybrid engine refuses to search unless the fingerprint matches its config.
var vectorTestGeneration = vector.Generation{
	ID: 1, Model: "test-model", Dimension: 4,
	Fingerprint: "test-model:4", State: vector.GenerationActive,
}

// saturatingFusingBackend is a vector.FusingBackend that returns a fixed hit
// list together with a caller-chosen saturation flag, so a test can pin what
// the eval path does with the flag the real engine computes. Only the two
// methods the hybrid search path calls are implemented; the embedded interface
// panics on anything else, which is the point — a widened call is a test bug,
// not something to silently stub.
type saturatingFusingBackend struct {
	vector.Backend

	generation vector.Generation
	hits       []vector.FusedHit
	saturated  bool
	fusedCalls int
}

func (b *saturatingFusingBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return b.generation, nil
}

func (b *saturatingFusingBackend) FusedSearch(
	context.Context, vector.FusedRequest,
) ([]vector.FusedHit, bool, error) {
	b.fusedCalls++
	return b.hits, b.saturated, nil
}

// stubEmbedder returns a fixed query vector; the fake backend never looks at
// it, but the engine insists on embedding before it will search.
type stubEmbedder struct{}

func (stubEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0, 0}, nil
}

// threadedCorpus builds a ranked corpus of threads*perThread messages, ordered
// thread by thread: the first perThread hits all belong to one conversation.
// That is the shape that breaks a truncate-then-collapse ranking.
func threadedCorpus(threads, perThread int) []store.APIMessage {
	out := make([]store.APIMessage, 0, threads*perThread)
	var id int64
	for t := range threads {
		for m := range perThread {
			id++
			out = append(out, store.APIMessage{
				ID:                   id,
				SourceMessageID:      fmt.Sprintf("<t%03d-m%d@example.com>", t, m),
				SourceConversationID: fmt.Sprintf("thread-%03d", t),
			})
		}
	}
	return out
}

// pagingFTS serves the first n results of corpus and records the depths it was
// asked for, so a test can assert both the answer and the work done to get it.
// It stands in for *store.Store on the production relevance-ranked FTS path.
type pagingFTS struct {
	corpus []store.APIMessage
	depths []int
}

func (p *pagingFTS) SearchMessagesQueryContext(
	_ context.Context, _ *search.Query, _, limit int,
) ([]store.APIMessage, int64, error) {
	p.depths = append(p.depths, limit)
	if limit > len(p.corpus) {
		limit = len(p.corpus)
	}
	return p.corpus[:limit], int64(len(p.corpus)), nil
}

func newPagingFTS(corpus []store.APIMessage) *pagingFTS {
	return &pagingFTS{corpus: corpus}
}

// evalTestLimit is the retrieval depth every ranking test uses; it matches the
// command's own -n default, so the over-fetch arithmetic in the assertions is
// the arithmetic a real run does.
const evalTestLimit = 100

func newTestEvaluator(t *testing.T, fts ftsSearcher, docKey string) (*evaluator, *runDiagnostics) {
	t.Helper()
	spec, ok := newDocKeyRegistry()[docKey]
	require.True(t, ok)
	diag := &runDiagnostics{}
	return &evaluator{ctx: t.Context(), fts: fts, key: spec, limit: evalTestLimit, diag: diag}, diag
}

// evalTestQuery parses a topic the way runEval does, so the ranking tests
// exercise the same already-validated query object the command builds.
func evalTestQuery(t *testing.T, qstr string) *search.Query {
	t.Helper()
	q := search.Parse(qstr)
	require.NoError(t, q.Err())
	return q
}

// TestRankedFTS_ConversationKeyFillsTheRequestedDepth is the regression for the
// ordering bug. The corpus has 4 messages per thread, so a raw fetch of 100
// covers only 25 threads. Collapsing after truncating returned those 25 and
// reported them as "R@100"; the ranking must instead over-fetch, collapse, and
// hand back 100 distinct threads.
func TestRankedFTS_ConversationKeyFillsTheRequestedDepth(t *testing.T) {
	corpus := threadedCorpus(200, 4)
	eng := newPagingFTS(corpus)
	ev, diag := newTestEvaluator(t, eng, "conversation")

	// For contrast, what the old ordering produced: collapsing the first
	// --limit raw hits only ever reached a quarter of the requested depth.
	firstPage := make([]string, 0, 100)
	for _, m := range corpus[:100] {
		firstPage = append(firstPage, m.SourceConversationID)
	}
	require.Len(t, eval.DedupeKeys(firstPage), 25, "truncate-then-collapse caps out at 25 threads here")

	ranked, err := ev.rankedFTS(evalTestQuery(t, "lease renewal"))
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
	eng := newPagingFTS(threadedCorpus(200, 5))
	ev, _ := newTestEvaluator(t, eng, "message")

	ranked, err := ev.rankedFTS(evalTestQuery(t, "lease renewal"))
	require.NoError(t, err)
	assert.Len(t, ranked, 100)
	assert.Equal(t, []int{100}, eng.depths, "no over-fetch for a 1:1 doc-key")
}

// TestRankedFTS_GrowsThePoolUntilTheDepthIsFilled: one over-fetch is not always
// enough. With 50 messages per thread, limit*4 still only covers 8 threads, so
// the pool has to grow.
func TestRankedFTS_GrowsThePoolUntilTheDepthIsFilled(t *testing.T) {
	eng := newPagingFTS(threadedCorpus(200, 50))
	ev, diag := newTestEvaluator(t, eng, "conversation")

	ranked, err := ev.rankedFTS(evalTestQuery(t, "lease renewal"))
	require.NoError(t, err)
	assert.Len(t, ranked, 100)
	assert.Equal(t, []int{400, 1600, 6400}, eng.depths, "the pool grows geometrically")
	assert.Zero(t, diag.DepthShortfalls, "the depth was filled on the last attempt")
}

// TestRankedFTS_StopsWhenTheEngineIsExhausted: a corpus smaller than the pool
// must not trigger pointless retries, and the short answer is not a shortfall
// worth warning about — there is simply nothing more to retrieve.
func TestRankedFTS_StopsWhenTheEngineIsExhausted(t *testing.T) {
	eng := newPagingFTS(threadedCorpus(6, 5)) // 30 messages, 6 threads
	ev, diag := newTestEvaluator(t, eng, "conversation")

	ranked, err := ev.rankedFTS(evalTestQuery(t, "lease renewal"))
	require.NoError(t, err)
	assert.Len(t, ranked, 6, "six threads exist; six threads come back")
	assert.Equal(t, []int{400}, eng.depths, "the engine came back short, so stop")
	assert.Zero(t, diag.DepthShortfalls)
	assert.Zero(t, diag.PoolShortfalls, "an unsaturated short page is an exhausted corpus, not a shortfall")
}

// TestRankedFTS_ReportsADepthShortfall: when even the largest pool cannot fill
// the depth, the metrics are computed over a shallower list than requested and
// the run has to say so rather than passing the number off as full depth.
func TestRankedFTS_ReportsADepthShortfall(t *testing.T) {
	// One single thread, deeper than the biggest pool: every fetch is
	// saturated, and every fetch collapses to one key.
	eng := newPagingFTS(threadedCorpus(1, 20_000))
	ev, diag := newTestEvaluator(t, eng, "conversation")

	ranked, err := ev.rankedFTS(evalTestQuery(t, "lease renewal"))
	require.NoError(t, err)
	assert.Equal(t, []string{"thread-000"}, ranked)
	assert.Equal(t, []int{400, 1600, 6400}, eng.depths, "the pool grows to the documented ceiling and stops")
	assert.Equal(t, 1, diag.DepthShortfalls)
	assert.Contains(t, diag.notes()[0], "could not fill")
}

// TestRankedKeys_SaturatedShortFetchIsNotCorpusExhaustion is the regression for
// the discarded PoolSaturated flag. A fused query caps each signal at
// k_per_signal, so it can return fewer hits than asked for while the corpus
// still holds plenty more. Reading that short page as "the corpus ran out"
// reported a pool-capped ranking as if it were everything retrieval could
// find — the one reading that makes the resulting metric silently wrong.
func TestRankedKeys_SaturatedShortFetchIsNotCorpusExhaustion(t *testing.T) {
	ev, diag := newTestEvaluator(t, nil, "conversation")
	diag.kPerSignal = 250

	var asked []int
	ranked, err := ev.rankedKeys(func(n int) (fetchResult, error) {
		asked = append(asked, n)
		// 40 hits back for a request of 400, spread over 10 threads, with
		// the engine reporting that its candidate pool was full.
		keys := make([]string, 0, 40)
		for i := range 40 {
			keys = append(keys, fmt.Sprintf("thread-%03d", i%10))
		}
		return fetchResult{keys: keys, raw: len(keys), saturated: true}, nil
	})
	require.NoError(t, err)

	assert.Len(t, ranked, 10, "the ten reachable threads are still scored")
	assert.Equal(t, []int{400}, asked,
		"a bigger page cannot get past k_per_signal, so do not burn another query on it")
	assert.Equal(t, 1, diag.PoolShortfalls)
	assert.Zero(t, diag.DepthShortfalls, "this is a pool ceiling, not an exhausted over-fetch budget")

	notes := diag.notes()
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "candidate pool")
	assert.Contains(t, notes[0], "k_per_signal=250", "the note names the setting that caused it")
	assert.Contains(t, notes[0], "not an exhausted corpus",
		"the whole point of the flag is to keep the two apart")
}

// TestRankedKeys_UnsaturatedShortFetchIsExhaustion pins the other half of the
// pair: the same short page, with the engine reporting it had nothing left,
// is not a shortfall at all and must stay silent.
func TestRankedKeys_UnsaturatedShortFetchIsExhaustion(t *testing.T) {
	ev, diag := newTestEvaluator(t, nil, "conversation")

	ranked, err := ev.rankedKeys(func(int) (fetchResult, error) {
		return fetchResult{keys: []string{"thread-000", "thread-001"}, raw: 2}, nil
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"thread-000", "thread-001"}, ranked)
	assert.Zero(t, diag.PoolShortfalls)
	assert.Zero(t, diag.DepthShortfalls)
	assert.Empty(t, diag.notes(), "an exhausted corpus is an answer, not an anomaly")
}

// TestRankedKeys_SaturatedFullDepthIsNotAShortfall: saturation only matters
// when the depth went unfilled. A run that got everything it asked for has
// nothing to warn about, however full the engine's pool was.
func TestRankedKeys_SaturatedFullDepthIsNotAShortfall(t *testing.T) {
	ev, diag := newTestEvaluator(t, nil, "conversation")

	keys := make([]string, 0, evalTestLimit)
	for i := range evalTestLimit {
		keys = append(keys, fmt.Sprintf("thread-%03d", i))
	}
	ranked, err := ev.rankedKeys(func(int) (fetchResult, error) {
		return fetchResult{keys: keys, raw: len(keys), saturated: true}, nil
	})
	require.NoError(t, err)

	assert.Len(t, ranked, evalTestLimit)
	assert.Zero(t, diag.PoolShortfalls)
	assert.Zero(t, diag.DepthShortfalls)
}

// TestRankedVector_CarriesPoolSaturationFromTheEngine checks the wiring the
// classification above depends on: hybrid.ResultMeta.PoolSaturated has to
// survive the trip from the engine into fetchResult. It used to be dropped on
// the floor at the call site, which no amount of correct classification
// downstream could recover from.
func TestRankedVector_CarriesPoolSaturationFromTheEngine(t *testing.T) {
	// One hit for a request of 400, with the backend reporting a full pool.
	backend := &saturatingFusingBackend{
		generation: vectorTestGeneration,
		hits:       []vector.FusedHit{{MessageID: 1, RRFScore: 0.9}},
		saturated:  true,
	}
	qeng := &querytest.MockEngine{
		GetMessageSummariesByIDsFunc: func(_ context.Context, ids []int64) ([]query.MessageSummary, error) {
			out := make([]query.MessageSummary, 0, len(ids))
			for _, id := range ids {
				out = append(out, query.MessageSummary{
					ID:                   id,
					SourceMessageID:      fmt.Sprintf("<m%d@example.com>", id),
					SourceConversationID: "thread-000",
				})
			}
			return out, nil
		},
	}
	ev, diag := newTestEvaluator(t, nil, "conversation")
	ev.qeng = qeng
	ev.heng = hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{
		ExpectedFingerprint: vectorTestGeneration.Fingerprint,
	})

	ranked, err := ev.rankedVector("hybrid", "lease renewal", evalTestQuery(t, "lease renewal"))
	require.NoError(t, err)

	assert.Equal(t, []string{"thread-000"}, ranked)
	assert.Equal(t, 1, backend.fusedCalls, "a saturated pool must not be retried at a deeper page")
	assert.Equal(t, 1, diag.PoolShortfalls, "the engine's own saturation flag has to reach the diagnostics")
	assert.Zero(t, diag.DepthShortfalls)
}

// TestRankedVector_FilterOnlyTopicIsRecoverable: a topic that parses to filters
// only has nothing to embed. It must surface as errNoFreeText — which runEval
// turns into a skipped cell — and never as an opaque failure that aborts the
// run and discards every score computed so far.
func TestRankedVector_FilterOnlyTopicIsRecoverable(t *testing.T) {
	const topic = "from:alice@example.com"
	parsed := evalTestQuery(t, topic)
	require.Empty(t, parsed.TextTerms, "fixture assumption: this topic is filter-only")

	// heng is deliberately nil: the check must happen before the engine is
	// ever touched.
	ev, _ := newTestEvaluator(t, nil, "message")
	ev.qeng = &querytest.MockEngine{}

	_, err := ev.rankedVector("vector", topic, parsed)
	require.Error(t, err)
	require.ErrorIs(t, err, errNoFreeText)
	assert.Contains(t, err.Error(), topic, "the error names the offending topic")
}

// TestParseTopic_RejectsAMalformedFilter is the regression for silently
// widened topics. search.Parse drops an operator value it cannot read and
// carries on, so `before:invalid renewal` becomes the unfiltered query
// `renewal` — a different question, scored under the original topic's id.
func TestParseTopic_RejectsAMalformedFilter(t *testing.T) {
	const bad = "before:invalid renewal"

	// What the old code would have run: the date filter is gone, and only
	// the bare term survives. This is the query that must NOT be scored.
	widened := search.Parse(bad)
	require.Error(t, widened.Err(), "fixture assumption: this topic does not parse cleanly")
	require.Nil(t, widened.BeforeDate, "the malformed filter is dropped, not honoured")
	require.Equal(t, []string{"renewal"}, widened.TextTerms, "leaving a strictly broader query behind")

	diag := &runDiagnostics{}
	q, ok := parseTopic(eval.Topic{ID: "q7", Query: bad}, diag)
	assert.False(t, ok, "a topic that does not parse must not be scored")
	assert.Nil(t, q)

	notes := diag.notes()
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "topic q7", "the report names the offending topic")
	assert.Contains(t, notes[0], "before", "and the operator that failed")
}

// TestParseTopic_AcceptsAWellFormedFilter: the guard rejects malformed values,
// not filters in general. A topic with a valid date filter parses through with
// the filter intact.
func TestParseTopic_AcceptsAWellFormedFilter(t *testing.T) {
	diag := &runDiagnostics{}
	q, ok := parseTopic(eval.Topic{ID: "q8", Query: "before:2024-01-01 renewal"}, diag)
	require.True(t, ok)
	require.NotNil(t, q)
	assert.NotNil(t, q.BeforeDate, "a filter that parses is kept")
	assert.Equal(t, []string{"renewal"}, q.TextTerms)
	assert.Empty(t, diag.notes(), "a clean topic says nothing")
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

	// The pool note stays readable when the run never opened the vector
	// path, so an unknown k_per_signal is never rendered as "k_per_signal=0".
	d = &runDiagnostics{PoolShortfalls: 2}
	require.Len(t, d.notes(), 1)
	assert.Contains(t, d.notes()[0], "candidate pool")
	assert.NotContains(t, d.notes()[0], "k_per_signal=0")
}
