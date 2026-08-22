//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

var (
	evalQrels  string
	evalTopics string
	evalModes  string
	evalDocKey string
	evalLimit  int
	evalJSON   bool
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Evaluate retrieval quality against relevance judgments (qrels)",
	Long: `Measure retrieval quality over a set of labeled queries.

Runs each topic through one or more search modes (fts, vector, hybrid) against
the local archive and scores the ranking against relevance judgments using
standard IR metrics: precision@10, nDCG@10, recall@100, MAP and MRR. This makes
the effect of an indexing, embedding, or fusion change measurable rather than
guessed. MAP and MRR are reported as MAP@n / MRR@n: they take no cutoff of
their own, but the ranking they score stops at -n, so a relevant message below
that rank is as invisible to them as it is to recall.

Inputs (TREC-style):
  --qrels   judgments file, one per line: "<qid> <iter> <docid> <rel>"
            (rel >= 1 means relevant; the iter column is ignored)
  --topics  queries file, tab-separated: "<qid>\t<query text>[\t<category>]"
            Each qid must appear once — it is the join key to --qrels, so a
            repeat would score the same judgments twice and weight that query
            twice over in every average; the file is rejected instead.
            The optional third column labels the question's shape (e.g.
            "pointed" for answerable-from-one-message, "spanning" for
            requires-synthesizing-across-messages); when present, results
            are also broken down per category. Two-column files work as-is.

Doc ids in --qrels are matched against each message's source_message_id by
default (--doc-key=message), or its conversation's source_conversation_id
(--doc-key=conversation). Pick the one your judgments actually reference: an
mbox import, for example, keys each imported document by conversation, so
message-keyed qrels would score a flat zero against it. When the judged unit
is the conversation, a thread is counted once, at its best-ranked message —
and retrieval over-fetches messages so that -n really does yield up to n
distinct threads. Reported latency therefore includes that over-fetch.

Both ids are assigned by the source and are unique only within it, while a
qrels doc id records no source at all. A run therefore stops before scoring
anything if the archive holds an id shared by two connected accounts: merging
two accounts' documents under one key would let an unjudged account's message
inherit a judged one's relevance. Accounts with disjoint id spaces (a mailbox
and a chat archive, say) score normally.

Metric depths follow -n: the standard P@10 / nDCG@10 / R@100 are reported when
the run retrieves at least that deep, and are clamped to -n below it (a run
that only ever looks 20 deep has no recall@100, and labelling one would invite
a false comparison). MAP and MRR are always at -n, since the truncated ranking
is the whole of what they see. The column headers always name the depth
actually used.

Each mode runs the same code production search runs. fts is the
relevance-ranked (BM25, subject-weighted) store path behind
/api/v1/search?mode=fts — not a chronological listing — so it honours the same
deletion scope and the same substring address-filter semantics real searches
do; vector and hybrid go through the hybrid engine with the fusion parameters
from your config. Query embedding follows [vector.embeddings] api_format, so an
index built through the Voyage contextual endpoint is queried through it too —
comparing "voyage-context-4" against an OpenAI-compatible model is a matter of
pointing the command at each config in turn.

Every run reports the embedding model, api format, index settings and index
size that produced it, plus per-query latency, because a quality number is not
comparable — or even interpretable — without them. Those numbers describe what
was searched, not what merely exists: the corpus count covers live messages
only (dedup-hidden duplicates and messages deleted from their source account
are excluded, as they are from every search here), and the vector count covers
the active generation only, not the retired ones vectors.db still holds. That
generation is resolved once at the start of the run; each query still searches
whatever is active at query time, so a rebuild or activation you trigger while
an eval is in flight can leave the reported vector count stale for topics
scored after the swap.
Anything that went wrong without being fatal — unparseable judgment lines,
topics whose query string did not parse or parsed to no search criteria at
all, hits that could not be hydrated from the archive, rankings cut short by
the fusion pool, topics no mode could score — is reported under "Diagnostics"
rather than silently folded into the scores. So is partial qrels coverage: a
topic the judgments never mention cannot be scored, and the diagnostics say
how many of the topics file that leaves the headline numbers standing on.

Note on topic phrasing: it is an experimental variable, not a constant. FTS5
matches on AND semantics, so a verbose natural-language topic requires every
one of its words to appear in a message and will usually score near zero,
while its keyword reduction scores well. Dense retrieval can move the other
way. Compare runs only across the same topics file.

Example:
  msgvault eval --qrels qrels.txt --topics topics.tsv --modes fts,vector,hybrid -n 100`,
	Args: cobra.NoArgs,
	RunE: runEval,
}

func init() {
	rootCmd.AddCommand(evalCmd)
	evalCmd.Flags().StringVar(&evalQrels, "qrels", "", "Path to TREC-format relevance judgments (required)")
	evalCmd.Flags().StringVar(&evalTopics, "topics", "", "Path to topics TSV: <qid>\\t<query> (required)")
	evalCmd.Flags().StringVar(&evalModes, "modes", "fts,vector,hybrid", "Comma-separated search modes to evaluate")
	// The registry's key set is fixed even though its entries are built per
	// run, so rendering the usage string from a throwaway registry is safe.
	evalCmd.Flags().StringVar(&evalDocKey, "doc-key", "message", "Which id qrels reference: "+docKeyNames(newDocKeyRegistry()))
	evalCmd.Flags().IntVarP(&evalLimit, "limit", "n", 100, "Distinct documents retrieved per query")
	evalCmd.Flags().BoolVar(&evalJSON, flagJSON, false, "Output as JSON")
	_ = evalCmd.MarkFlagRequired("qrels")
	_ = evalCmd.MarkFlagRequired("topics")
}

// errNoFreeText marks a topic that vector and hybrid modes structurally cannot
// answer: it parsed to filters only, so there is nothing to embed. It is a
// property of one topic, not of the run, so it is reported per cell instead of
// aborting and discarding every score computed so far.
var errNoFreeText = errors.New("topic has no free-text terms to embed")

// evalHit is the doc-key-relevant projection of one retrieved message.
//
// The retrieval paths disagree about what a hit is: the store's
// relevance-ranked FTS path returns store.APIMessage, while the vector path
// hydrates hybrid hits into query.MessageSummary. A --doc-key has to mean the
// same thing whichever engine produced the hit, so both paths project into
// this one struct and the doc-key registry is defined over it alone.
type evalHit struct {
	// MessageID is the archive's own row id. No registered doc-key uses it
	// yet; it is carried because the judged-unit extension the registry
	// documents (a reconstructed-thread id resolved through an external
	// mapping) resolves from an id, not from text.
	MessageID            int64
	SourceMessageID      string
	SourceConversationID string
}

// hitFromAPIMessage projects a store-path (relevance-ranked FTS) result.
func hitFromAPIMessage(m store.APIMessage) evalHit {
	return evalHit{
		MessageID:            m.ID,
		SourceMessageID:      m.SourceMessageID,
		SourceConversationID: m.SourceConversationID,
	}
}

// hitFromSummary projects a hydrated vector/hybrid result.
func hitFromSummary(m query.MessageSummary) evalHit {
	return evalHit{
		MessageID:            m.ID,
		SourceMessageID:      m.SourceMessageID,
		SourceConversationID: m.SourceConversationID,
	}
}

// evalIDColumn locates, in the archive, the column a doc-key's ids are read
// from. It exists so the cross-source collision check
// (requireDisjointSourceIDs) can be written once, over any key, without having
// to know what a particular key means.
type evalIDColumn struct {
	// table is the alias the collision query gives the id's table: "m" for
	// messages, "c" for the conversations reached through join.
	table string
	// column is the unqualified column name, and doubles as the name the
	// error text shows a user.
	column string
	// join is the extra FROM clause needed to reach table, empty when the id
	// lives on messages itself.
	join string
}

// expr renders the qualified column for use in the collision query.
func (c evalIDColumn) expr() string { return c.table + "." + c.column }

// docKeySpec describes one --doc-key value.
type docKeySpec struct {
	// extract pulls, from a retrieved hit, the stable document id that qrels
	// judge against. It is the only place a doc-key's meaning lives: the
	// scoring core (eval.Evaluate, eval.Aggregate, eval.DedupeKeys) operates
	// on the opaque string keys these return and never learns what they
	// identify.
	extract func(evalHit) string
	// collapses is true when the judged unit is coarser than the retrieved
	// one, so several hits routinely fold into a single key. Retrieval then
	// has to over-fetch messages to fill the requested depth with *distinct*
	// keys — see eval.OverFetchPlan and evaluator.rankedKeys.
	collapses bool
	// idColumn says where in the archive extract's id comes from, so the run
	// can check up front that no id in it names documents in two different
	// connected sources — see requireDisjointSourceIDs, which refuses to run a
	// key that leaves this unset rather than skipping the check.
	idColumn evalIDColumn
}

// newDocKeyRegistry builds the --doc-key registry for one run.
//
// It is a constructor rather than a package-level map so the registry is built
// after flags are parsed. That is what makes the extension story real: a
// future --doc-key=thread, resolving a reconstructed-thread id through an
// externally supplied message-id -> thread-id mapping file, is one more entry
// here, closing over the mapping this function loaded. A map initialised at
// program start could not hold that entry — the mapping file is named by a
// flag, and its contents are unknown until runEval runs. The CLI validation,
// the scoring core and the output paths all pick a new entry up unchanged.
func newDocKeyRegistry() map[string]docKeySpec {
	return map[string]docKeySpec{
		// One message, one source_message_id. Duplicates are still collapsed
		// (the same message synced from two accounts), but they are rare
		// enough that the depth does not need padding for them.
		"message": {
			extract:  func(h evalHit) string { return h.SourceMessageID },
			idColumn: evalIDColumn{table: "m", column: "source_message_id"},
		},
		// Many messages share one conversation, so filling n distinct threads
		// takes more than n messages.
		"conversation": {
			extract:   func(h evalHit) string { return h.SourceConversationID },
			collapses: true,
			idColumn: evalIDColumn{
				table:  "c",
				column: "source_conversation_id",
				join:   "JOIN conversations c ON c.id = m.conversation_id",
			},
		},
	}
}

// docKeyNames renders the valid --doc-key values for usage and error text.
func docKeyNames(registry map[string]docKeySpec) string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// runDiagnostics collects the non-fatal anomalies of a run. Each of these was
// previously either silent or fatal, and both are wrong for an instrument: a
// stale index that quietly drops hits is precisely the failure this command
// exists to expose, and a single unanswerable topic should not throw away
// every other topic's score.
type runDiagnostics struct {
	QrelsLoad      eval.LoadStats `json:"qrels_load"`
	TopicsLoad     eval.LoadStats `json:"topics_load"`
	UnhydratedHits int            `json:"unhydrated_hits,omitempty"`
	// DepthShortfalls counts runs that ran out of over-fetch budget with the
	// engine still willing to give more. PoolShortfalls counts runs that hit
	// the hybrid engine's candidate-pool ceiling instead — the engine came
	// back short, but more matching messages exist beyond the pool. The two
	// are kept apart because only one of them is fixable by configuration,
	// and neither may be confused with "the corpus genuinely ran out", which
	// is not a shortfall at all.
	DepthShortfalls int      `json:"depth_shortfalls,omitempty"`
	PoolShortfalls  int      `json:"pool_shortfalls,omitempty"`
	SkippedCells    []string `json:"skipped_cells,omitempty"`
	// UnjudgedTopics names the topics the qrels file says nothing about.
	// Leaving them out of the scoring is correct — an unjudged topic has
	// nothing to be scored against — but leaving it *unsaid* is not: as soon
	// as one topic is judged the run reports a headline number, and a qrels
	// file that matches only a handful of a large topics file therefore
	// reports it over a small, self-selected subset while looking like a
	// complete run. Naming them makes the coverage of a run readable from its
	// own output.
	UnjudgedTopics []string `json:"unjudged_topics,omitempty"`

	// kPerSignal is the fusion pool size in force for this run, used only to
	// make the PoolShortfalls note actionable. Unexported so it stays out of
	// the JSON diagnostics block, where it would duplicate run_config.
	kPerSignal int

	// scored is how many topics actually contributed a score, set once after
	// the scoring loop finishes. It is not len(TopicsLoad)-len(UnjudgedTopics):
	// a judged topic can still fail to score (an empty parsed query, or every
	// mode skipping it), so the unjudged-topics note must read this field
	// rather than recompute a count the loop already produced correctly —
	// the same number the report's own topics_evaluated is built from.
	// Unexported for the same reason as kPerSignal: it would duplicate
	// topics_evaluated in the JSON diagnostics block.
	scored int
}

// skip records that one topic/mode combination could not be scored.
func (d *runDiagnostics) skip(topicID, mode, reason string) {
	d.SkippedCells = append(d.SkippedCells, fmt.Sprintf("topic %s / %s: %s", topicID, mode, reason))
}

// skipTopic records that a topic could not be scored by any mode. Unlike skip,
// this is a property of the topic itself (a query string no mode can run), so
// it is reported once rather than once per mode.
func (d *runDiagnostics) skipTopic(topicID, reason string) {
	d.SkippedCells = append(d.SkippedCells, fmt.Sprintf("topic %s: %s", topicID, reason))
}

// unjudged records a topic this qrels file never mentions. It is not a skip:
// nothing went wrong with the topic, there is simply nothing to score it
// against. It is tracked so the run can report how much of the topics file its
// headline numbers actually cover.
func (d *runDiagnostics) unjudged(topicID string) {
	d.UnjudgedTopics = append(d.UnjudgedTopics, topicID)
}

// notes renders the diagnostics as human-readable lines, empty when the run
// was clean.
func (d *runDiagnostics) notes() []string {
	var out []string
	for _, l := range []struct {
		kind  string
		stats eval.LoadStats
	}{{"qrels", d.QrelsLoad}, {"topics", d.TopicsLoad}} {
		if l.stats.Skipped > 0 {
			out = append(out, fmt.Sprintf("%s %s: %s — skipped lines did not match the expected format",
				l.kind, l.stats.Path, l.stats))
		}
	}
	// Partial coverage is not an error — a topics file is often larger than
	// the judgments gathered for it so far — but it changes what the headline
	// numbers mean, so it is stated rather than inferred from the topic count.
	if n := len(d.UnjudgedTopics); n > 0 {
		// d.scored, not d.TopicsLoad.Parsed-n: a judged topic can still fail
		// to score (an empty parsed query, every mode skipping it), so the
		// remainder after subtracting only the unjudged ones overstates what
		// the run actually covers — see the scored field's own doc comment.
		out = append(out, fmt.Sprintf(
			"%d of %d topics had no matching qrels entry and were not scored (%s); "+
				"the reported metrics cover %d of the topics file, so they describe a subset of %s — "+
				"check that the qids in both files refer to the same queries",
			n, d.TopicsLoad.Parsed, eval.FormatIDList(d.UnjudgedTopics, 10),
			d.scored, d.TopicsLoad.Path))
	}
	if d.UnhydratedHits > 0 {
		out = append(out, fmt.Sprintf(
			"%d retrieved hits could not be hydrated back to a message row and were dropped from the ranking; "+
				"this usually means the vector index references deleted or unmigrated messages (re-run `msgvault embed`)",
			d.UnhydratedHits))
	}
	if d.DepthShortfalls > 0 {
		out = append(out, fmt.Sprintf(
			"%d topic/mode runs could not fill %d distinct %s keys within the over-fetch budget (%dx -n); "+
				"their metrics are computed over a shallower list than requested",
			d.DepthShortfalls, evalLimit, evalDocKey, eval.MaxOverFetchFactor))
	}
	if d.PoolShortfalls > 0 {
		out = append(out, fmt.Sprintf(
			"%d topic/mode runs stopped short of %d distinct %s keys because the fusion candidate pool "+
				"saturated%s: the engine returned fewer hits than asked for, but more matching messages "+
				"exist beyond the pool — this is a reachability limit, not an exhausted corpus. "+
				"Raise [vector.search].k_per_signal to rank deeper, and note that doing so changes the "+
				"fusion, so only compare runs at the same setting",
			d.PoolShortfalls, evalLimit, evalDocKey, kPerSignalSuffix(d.kPerSignal)))
	}
	out = append(out, d.SkippedCells...)
	return out
}

// kPerSignalSuffix renders the fusion pool size for a diagnostic line, or
// nothing when the run never opened the vector path (so the number is unknown
// rather than zero).
func kPerSignalSuffix(k int) string {
	if k <= 0 {
		return ""
	}
	return fmt.Sprintf(" at k_per_signal=%d", k)
}

// ftsSearcher is the production relevance-ranked full-text path.
//
// It is deliberately the Store's search, not query.Engine.Search: those are
// two different searches. query.Engine.Search returns matches in reverse
// chronological order with no relevance component at all, so scoring it as a
// *ranking* measures the archive's date distribution rather than its retrieval
// quality. Store.SearchMessagesQueryContext is the path /api/v1/search?mode=fts
// serves, ordering by the dialect's BM25 expression (subject-weighted) before
// falling back to recency — the same messages_fts index and the same weights
// the hybrid engine's BM25 leg fuses. It also matches production on the two
// semantics that silently move scores: it honours search.DeletionScope
// (active-only by default, so source-deleted messages are excluded), and its
// from:/to:/cc: filters are substring matches rather than exact-address
// equality.
//
// *store.Store satisfies this; the interface exists so tests can drive the
// over-fetch loop without a database.
type ftsSearcher interface {
	SearchMessagesQueryContext(
		ctx context.Context, q *search.Query, offset, limit int,
	) ([]store.APIMessage, int64, error)
}

// evaluator bundles the engines and config needed to turn a query string into
// a ranked list of document ids for a given search mode.
type evaluator struct {
	ctx   context.Context
	fts   ftsSearcher
	qeng  query.Engine
	heng  *hybrid.Engine
	key   docKeySpec
	limit int
	prov  eval.RunConfig
	diag  *runDiagnostics
}

// fetchResult is one attempt at pulling raw hits out of a search engine.
type fetchResult struct {
	// keys are the doc keys in the engine's rank order, before collapsing;
	// duplicates and empty strings are expected and handled by the caller.
	keys []string
	// raw is how many hits the engine returned. It is the count *before* key
	// extraction, so "the engine gave back fewer than we asked for" — the
	// signal that a deeper fetch cannot help — stays accurate even when some
	// hits fail to hydrate.
	raw int
	// dropped is how many hits could not be hydrated back to a message row.
	dropped int
	// saturated reports that the engine filled its own candidate pool: it
	// had at least one more candidate than it was willing to consider. It is
	// what separates "the corpus ran out" from "the engine stopped looking",
	// which look identical from the hit count alone. The hybrid engine's
	// fused query caps each signal at k_per_signal, so it can hand back fewer
	// hits than requested while the corpus still holds plenty more — see
	// hybrid.ResultMeta.PoolSaturated.
	saturated bool
}

// rankedKeys turns a search engine into up to limit *distinct* doc keys, in
// ranked order.
//
// The collapse must happen before the truncation, not after: retrieving n
// messages and then collapsing them yields however many distinct threads
// happen to sit inside those n, which is not what "-n 100" claims. So for a
// collapsing doc-key this over-fetches raw hits (eval.OverFetchPlan), collapses,
// and only then cuts to the requested depth, growing the pool while the engine
// still has more to give and the depth is still unfilled.
func (e *evaluator) rankedKeys(fetch func(n int) (fetchResult, error)) ([]string, error) {
	plan := eval.OverFetchPlan(e.limit, e.key.collapses)
	for i, n := range plan {
		res, err := fetch(n)
		if err != nil {
			return nil, err
		}
		deduped := eval.DedupeKeys(res.keys)
		filled := len(deduped) >= e.limit
		// The engine came back short. Why it came back short decides both
		// whether to retry and what to report, and the hit count alone cannot
		// tell the two apart:
		//
		//   - not saturated: it gave everything it had. The corpus is
		//     exhausted, a deeper fetch returns the same list, and a short
		//     ranking is the honest answer — not a shortfall.
		//   - saturated: it filled its own candidate pool and stopped. More
		//     matching messages exist, but no value of n reaches them,
		//     because the ceiling is k_per_signal, not the page size. Retrying
		//     deeper would only burn queries, so stop — and say so, because
		//     scoring this as an exhausted corpus reports a shallow ranking
		//     as if it were the whole of what retrieval could find.
		short := res.raw < n
		exhausted := short && !res.saturated
		poolCapped := short && res.saturated
		if filled || exhausted || poolCapped || i == len(plan)-1 {
			e.diag.UnhydratedHits += res.dropped
			switch {
			case filled || exhausted:
				// Nothing to report: the depth was met, or there was
				// genuinely nothing more to retrieve.
			case poolCapped:
				e.diag.PoolShortfalls++
			default:
				e.diag.DepthShortfalls++
			}
			return eval.TruncateKeys(deduped, e.limit), nil
		}
	}
	// Unreachable: OverFetchPlan is never empty and the loop always returns on
	// its final step.
	return nil, nil
}

// rankedFTS scores the production relevance-ranked FTS path. See ftsSearcher
// for why that is the Store's search and not query.Engine.Search.
func (e *evaluator) rankedFTS(q *search.Query) ([]string, error) {
	return e.rankedKeys(func(n int) (fetchResult, error) {
		res, _, err := e.fts.SearchMessagesQueryContext(e.ctx, q, 0, n)
		if err != nil {
			return fetchResult{}, err
		}
		keys := make([]string, 0, len(res))
		for _, m := range res {
			keys = append(keys, e.key.extract(hitFromAPIMessage(m)))
		}
		// The store path pages a single ranked list, so a short page means
		// the corpus ran out — there is no candidate pool to saturate.
		return fetchResult{keys: keys, raw: len(res)}, nil
	})
}

func (e *evaluator) rankedVector(mode, qstr string, q *search.Query) ([]string, error) {
	// Both modes embed the free text, so a filter-only topic (`from:alice`)
	// has nothing to embed and hybrid.Engine.Search would return a bare
	// "empty query". Detect it here and hand runEval a recognisable error so
	// it can skip this one cell instead of aborting the run.
	if len(q.TextTerms) == 0 {
		return nil, fmt.Errorf("%w: %q parsed to filters only", errNoFreeText, qstr)
	}
	// Use the engine method rather than the package function: it supplies the
	// dialect's placeholder rebind, which the package function now requires.
	filter, err := e.heng.BuildFilter(e.ctx, q)
	if err != nil {
		return nil, fmt.Errorf("build filter: %w", err)
	}
	subjectTerms := make([]string, 0, len(q.TextTerms))
	for _, t := range q.TextTerms {
		subjectTerms = append(subjectTerms, strings.ToLower(t))
	}
	freeText := strings.Join(q.TextTerms, " ")

	return e.rankedKeys(func(n int) (fetchResult, error) {
		hits, meta, err := e.heng.Search(e.ctx, hybrid.SearchRequest{
			Mode:         hybrid.Mode(mode),
			FreeText:     freeText,
			Filter:       filter,
			Limit:        n,
			SubjectTerms: subjectTerms,
		})
		if err != nil {
			return fetchResult{}, err
		}
		if len(hits) == 0 {
			return fetchResult{saturated: meta.PoolSaturated}, nil
		}
		ids := make([]int64, len(hits))
		for i, h := range hits {
			ids[i] = h.MessageID
		}
		summaries, err := e.qeng.GetMessageSummariesByIDs(e.ctx, ids)
		if err != nil {
			return fetchResult{}, fmt.Errorf("map message ids: %w", err)
		}
		byID := make(map[int64]query.MessageSummary, len(summaries))
		for _, m := range summaries {
			byID[m.ID] = m
		}
		// Preserve the engine's ranking order. A hit that cannot be hydrated
		// is dropped — but counted, because a vector index pointing at rows
		// the archive no longer has is exactly the staleness this command
		// exists to surface.
		out := fetchResult{
			keys: make([]string, 0, len(hits)),
			raw:  len(hits),
			// Carry the engine's own account of why it stopped. Without
			// it, a fused query that ran out of candidate pool is
			// indistinguishable from one that ran out of corpus, and
			// rankedKeys would report a pool-capped ranking as complete.
			saturated: meta.PoolSaturated,
		}
		for _, h := range hits {
			m, ok := byID[h.MessageID]
			if !ok {
				out.dropped++
				continue
			}
			out.keys = append(out.keys, e.key.extract(hitFromSummary(m)))
		}
		return out, nil
	})
}

// ranked runs one topic through one mode. The topic is parsed once by the
// caller and handed in already validated: re-parsing per mode would let the
// same malformed filter be dropped silently three times over.
func (e *evaluator) ranked(mode, qstr string, q *search.Query) ([]string, error) {
	switch mode {
	case "fts":
		return e.rankedFTS(q)
	case "vector", "hybrid":
		return e.rankedVector(mode, qstr, q)
	default:
		return nil, fmt.Errorf("unknown mode %q (want fts|vector|hybrid)", mode)
	}
}

// parseTopic turns one topic's query string into a validated search.Query.
//
// search.Parse never fails outright: an operator it recognises but cannot
// read — `before:invalid`, `larger:5X` — is recorded on the query and the
// filter is simply dropped, leaving a *wider* query behind. That is a
// reasonable default for an interactive search box, where the user sees the
// results and can correct the typo, but it is silent corruption for a
// benchmark: the topic still scores, against a question nobody asked. The
// production front doors (the CLI search command, /api/v1/search,
// /cli/search) all reject such a query via Query.Err(); this one skips the
// topic and says why, so one malformed line cannot quietly move a run's
// headline numbers.
//
// Parsing cleanly is not enough, though: a topic can be non-empty text and
// still parse to no search criteria at all. `subject:""` is the plain case —
// the parser drops an empty operator value rather than building a `LIKE '%%'`
// that matches everything — and the widest possible query is what is left
// behind. That is the same corruption one step further along, and it is worse,
// because the fts path answers an empty query by listing the whole live corpus
// in its default order: the topic scores whatever the archive's date
// distribution happens to give it. Production rejects the identical query
// (cmd/search.go and the /cli/search handler both test Query.IsEmpty), so it
// is skipped and reported here too.
func parseTopic(t eval.Topic, diag *runDiagnostics) (*search.Query, bool) {
	q := search.Parse(t.Query)
	if err := q.Err(); err != nil {
		diag.skipTopic(t.ID, fmt.Sprintf("query %q did not parse: %v — scoring it would have "+
			"silently evaluated the broader query left after the bad filter was dropped", t.Query, err))
		return nil, false
	}
	if q.IsEmpty() {
		diag.skipTopic(t.ID, fmt.Sprintf("query %q parsed to no search criteria at all — scoring it "+
			"would have ranked the whole live corpus in its default order rather than a retrieval "+
			"of this topic; production search rejects the same query as empty", t.Query))
		return nil, false
	}
	return q, true
}

func runEval(cmd *cobra.Command, _ []string) error {
	registry := newDocKeyRegistry()
	keySpec, ok := registry[evalDocKey]
	if !ok {
		return usageErr(cmd, fmt.Errorf("invalid --doc-key %q (want %s)", evalDocKey, docKeyNames(registry)))
	}
	// A non-positive depth is not a "use the default" signal: fts would fall
	// back to an internal 100 while the vector backend would return nothing
	// for k=0, so the same flag would mean two different things. Reject it.
	if evalLimit <= 0 {
		return usageErr(cmd, fmt.Errorf("--limit must be a positive integer, got %d", evalLimit))
	}
	modes, needVec, err := parseEvalModes(evalModes)
	if err != nil {
		return usageErr(cmd, err)
	}
	cutoffs := eval.CutoffsForDepth(evalLimit)

	diag := &runDiagnostics{}
	qrels, qrelsStats, err := eval.LoadQrels(evalQrels)
	if err != nil {
		return err
	}
	diag.QrelsLoad = qrelsStats
	topics, topicsStats, err := eval.LoadTopics(evalTopics)
	if err != nil {
		return err
	}
	diag.TopicsLoad = topicsStats

	// A file in a near-miss format parses to an empty-but-valid result. Say so
	// in terms of the format, because the downstream symptom ("no topics had
	// judgments") reads like an id mismatch and sends people looking in the
	// wrong place.
	if qrelsStats.Parsed == 0 {
		return fmt.Errorf("no judgments parsed from %s (%s); expected whitespace-separated "+
			"\"<qid> <iter> <docid> <rel>\" — a three-column file without the iteration column is the usual cause",
			evalQrels, qrelsStats)
	}
	if len(topics) == 0 {
		return fmt.Errorf("no topics loaded from %s (%s); expected tab-separated "+
			"\"<qid>\\t<query text>\" — spaces where tabs are expected is the usual cause",
			evalTopics, topicsStats)
	}
	// Warn on stderr so --json output stays machine-readable.
	for _, l := range []struct {
		kind  string
		stats eval.LoadStats
	}{{"qrels", qrelsStats}, {"topics", topicsStats}} {
		if l.stats.Suspect() {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s %s parsed oddly (%s); check the file format\n",
				l.kind, l.stats.Path, l.stats)
		}
	}

	ctx := cmd.Context()

	// Store + query engine: serves FTS search and the rowid -> source-id
	// mapping for vector/hybrid hits. Opening it also runs the schema
	// migrations the vector backend relies on, and its handle is the one every
	// DB read in this command goes through.
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(s); err != nil {
		return fmt.Errorf("startup migrations: %w", err)
	}
	// Every score this run produces rests on one archive-wide fact about the
	// chosen key: that its ids name one document each. Establish it before the
	// vector path is opened and before the first topic is scored, so a run that
	// cannot be trusted stops instead of printing a number.
	if err := requireDisjointSourceIDs(ctx, s.DB(), evalDocKey, keySpec); err != nil {
		return err
	}

	ev := &evaluator{
		ctx: ctx,
		// The store serves --modes fts through the same relevance-ranked
		// path /api/v1/search?mode=fts uses; the query engine serves the
		// rowid -> source-id hydration the vector/hybrid path needs.
		fts:   s,
		qeng:  query.NewEngine(s.DB(), s.IsPostgreSQL()),
		key:   keySpec,
		limit: evalLimit,
		diag:  diag,
	}

	if needVec {
		cleanup, err := ev.attachVector(ctx, s)
		if err != nil {
			return err
		}
		defer cleanup()
	}

	// Record corpus size regardless of mode: recall numbers are unreadable
	// without knowing how big the haystack was.
	ev.prov.QrelsPath = evalQrels
	ev.prov.TopicsPath = evalTopics
	ev.collectCorpusStats(s.DB())

	aggs := make(map[string]*eval.Aggregate, len(modes))
	lats := make(map[string]*eval.LatencyTracker, len(modes))
	for _, m := range modes {
		aggs[m] = &eval.Aggregate{}
		lats[m] = &eval.LatencyTracker{}
	}
	// Per-category aggregates (mode -> category), populated only for topics
	// that carry a category label. Whether a question is answerable from one
	// message or needs a whole thread decides which retrieval levers a run
	// can even see, so when the topics file says which is which, report the
	// split rather than averaging it away.
	catAggs := make(map[string]map[string]*eval.Aggregate, len(modes))
	catCounts := map[string]int{}
	scored := 0
	for _, t := range topics {
		if !qrels.HasJudgments(t.ID) {
			// This qrels file says nothing at all about the topic, so there is
			// nothing to score it against. Record it: one judged topic is
			// enough to produce a headline number, and a reader has to be able
			// to see how much of the topics file that number covers.
			diag.unjudged(t.ID)
			continue
		}
		// May legitimately be empty: a topic judged but with every document
		// graded non-relevant scores a real zero and belongs in the macro
		// average. Dropping it would quietly raise every reported mean — see
		// eval.Qrels.HasJudgments.
		rel := qrels.RelevantSet(t.ID)
		// Parse once, before any mode runs it: a malformed filter is a
		// property of the topic, not of the mode, and must not be silently
		// widened into a different question three times over.
		q, ok := parseTopic(t, diag)
		if !ok {
			continue
		}
		anyMode := false
		for _, m := range modes {
			start := time.Now()
			ranked, err := ev.ranked(m, t.Query, q)
			elapsed := time.Since(start)
			if err != nil {
				if errors.Is(err, errNoFreeText) {
					// One mode cannot answer this topic. Record it and carry
					// on: aborting here would throw away every score already
					// computed, for every mode and every earlier topic.
					diag.skip(t.ID, m, "no free-text terms to embed (filter-only topic)")
					continue
				}
				return fmt.Errorf("topic %s, mode %s: %w", t.ID, m, err)
			}
			lats[m].Add(elapsed)
			s := eval.Evaluate(ranked, rel, cutoffs)
			aggs[m].Add(s)
			if t.Category != "" {
				if catAggs[m] == nil {
					catAggs[m] = map[string]*eval.Aggregate{}
				}
				if catAggs[m][t.Category] == nil {
					catAggs[m][t.Category] = &eval.Aggregate{}
				}
				catAggs[m][t.Category].Add(s)
			}
			anyMode = true
		}
		if !anyMode {
			continue // no mode could score this topic
		}
		if t.Category != "" {
			catCounts[t.Category]++
		}
		scored++
	}
	diag.scored = scored
	if scored == 0 {
		// Distinguish the two ways a run can end up with nothing: no topic
		// matched a judgment, or every topic was skipped by every mode.
		if len(diag.SkippedCells) > 0 {
			return fmt.Errorf("no topic could be scored by any of the requested modes: %s",
				strings.Join(diag.SkippedCells, "; "))
		}
		return fmt.Errorf("none of the %d topics had relevance judgments in %s "+
			"(qrels: %s; topics: %s); check that the qids in both files refer to the same queries",
			len(topics), evalQrels, qrelsStats, topicsStats)
	}

	report := evalReport{
		modes: modes, aggs: aggs, lats: lats, catAggs: catAggs, catCounts: catCounts,
		prov: ev.prov, topics: scored, cutoffs: cutoffs, diag: diag,
	}
	if evalJSON {
		return report.json()
	}
	report.table()
	return nil
}

// parseEvalModes splits and validates the --modes flag, keeping each mode once
// in the order it was first named.
//
// A repeated mode is deduplicated rather than rejected. The scoring loop
// evaluates and aggregates the list entry by entry into a per-mode Aggregate,
// so `--modes fts,fts` would run every topic through fts twice, add each score
// to the same aggregate twice — doubling that mode's topic count while leaving
// its means unchanged — and double the latency work and its sample. Rejecting
// it, as a repeated qid in the topics file is rejected, would be the wrong
// shape here: two topic rows sharing a qid carry different query text, so the
// file is genuinely ambiguous and picking one silently answers a question
// nobody asked, whereas `fts,fts` has exactly one possible reading. There is
// nothing to disambiguate, so it is simply honoured once.
//
// Validation still runs per entry, before the duplicate is dropped, so a
// repeated invalid mode is still an error. Order is preserved because it is the
// order the report's rows come out in, and that belongs to the user.
func parseEvalModes(spec string) (modes []string, needVec bool, err error) {
	seen := make(map[string]bool, 3)
	for m := range strings.SplitSeq(spec, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		switch m {
		case "fts":
		case "vector", "hybrid":
			needVec = true
		default:
			return nil, false, fmt.Errorf("invalid mode %q in --modes (want fts|vector|hybrid)", m)
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		modes = append(modes, m)
	}
	if len(modes) == 0 {
		return nil, false, errors.New("--modes is empty")
	}
	return modes, needVec, nil
}

// attachVector wires the sqlite-vec backend and hybrid engine onto the
// evaluator (mirroring the search command's vector path) and returns a
// cleanup closure that closes the resources it opened.
//
// It reuses the caller's store handle rather than opening its own: that handle
// already carries the DSN parameters store.Open applies (busy_timeout, WAL,
// the registered driver's unicode_lower hook), and routing every DB operation
// through the Store is this repo's rule.
func (e *evaluator) attachVector(ctx context.Context, mainStore *store.Store) (func(), error) {
	if !cfg.Vector.Enabled {
		return nil, errors.New("vector/hybrid modes need [vector].enabled = true in config")
	}
	mainPath := cfg.DatabaseDSN()
	if store.IsPostgresURL(mainPath) {
		// This command's vector path is the sqlite-vec one; a PG archive
		// stores its embeddings in pgvector, alongside the messages. Fail
		// clearly rather than pointing a sqlite-vec backend at a PG handle.
		return nil, errors.New("vector/hybrid eval currently supports SQLite archives only; " +
			"the configured database is PostgreSQL — run with --modes fts")
	}

	// Resolve [vector.embed.scope] accounts to source IDs before deriving the
	// build scope or the generation fingerprint, exactly as the serve/embed
	// paths do. The fingerprint folds in the scope, so an unresolved config
	// would compute a different one and every query would fail as "index
	// stale" on any archive that scopes embedding by account.
	vecCfg, err := resolvedVectorConfig(mainStore)
	if err != nil {
		return nil, fmt.Errorf("vector embed scope: %w", err)
	}
	// Validate the resolved config with the same check serve runs before it
	// opens anything. It names the offending key and value — including an
	// api_format this binary has no client for, which must fail here rather
	// than fall back to a client that talks a different protocol to the
	// endpoint that built the index.
	if err := vecCfg.Validate(); err != nil {
		return nil, fmt.Errorf("vector/hybrid modes need a valid [vector] config: %w", err)
	}

	// Select the query client by api_format, exactly as the serve path does,
	// and before anything is opened. A run scored with the OpenAI-compatible
	// client against a voyage-contextual index would measure a protocol
	// mismatch, not retrieval quality. Every eval call is query-time, and each
	// client's EmbedQuery carries its own query role (Voyage sends
	// input_type=query to /contextualizedembeddings), so no document-side
	// wiring is needed here.
	embedClient, err := newQueryEmbeddingClient(vecCfg)
	if err != nil {
		return nil, err
	}
	mainDB := mainStore.DB()

	vecDBPath := vecCfg.DBPath
	if vecDBPath == "" {
		vecDBPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	if err := sqlitevec.RegisterExtension(); err != nil {
		return nil, fmt.Errorf("register sqlite-vec: %w", err)
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:       vecDBPath,
		MainPath:   mainPath,
		Dimension:  vecCfg.Embeddings.Dimension,
		MainDB:     mainDB,
		BuildScope: vecCfg.Embed.Scope.BuildScope(),
	})
	if err != nil {
		return nil, fmt.Errorf("open vectors.db: %w", err)
	}
	// Keep the resolved generation: it is the one the hybrid engine will
	// search, and therefore the only one whose vector count describes this
	// run's index.
	active, err := vector.ResolveActiveForFingerprint(ctx, backend, vecCfg.GenerationFingerprint())
	if err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("resolve active generation: %w", err)
	}

	e.heng = hybrid.NewEngine(backend, mainDB, embedClient, hybrid.Config{
		ExpectedFingerprint: vecCfg.GenerationFingerprint(),
		RRFK:                vecCfg.Search.RRFK,
		KPerSignal:          vecCfg.Search.KPerSignal,
		SubjectBoost:        vecCfg.Search.SubjectBoost,
		// Without this the engine's index-scope check short-circuits to nil,
		// so an out-of-scope filter would run against an index holding no
		// vectors for that scope and its near-zero hit count would be scored
		// as genuinely poor retrieval instead of erroring.
		BuildScope: vecCfg.Embed.Scope.BuildScope(),
	})
	e.collectVectorStats(mainDB, vecCfg, backend, vecDBPath, active.ID)

	return func() { _ = backend.Close() }, nil
}

// collectCorpusStats records how big the searched archive is. It reads the
// store's own handle so it also works for --modes fts, which never opens the
// vector path. Failures are non-fatal: missing provenance should degrade the
// report, never abort a run.
//
// "How big" means how big the haystack retrieval actually searched, not how
// many rows the tables hold. A long-lived archive accumulates dedup-hidden
// duplicates and messages deleted from their source account, and no search
// this command runs returns either: the fts path resolves the default active
// deletion scope to store.LiveMessagesWhere, and the vector path drops
// source-deleted hits after the fact. Counting them would overstate the
// haystack and make recall look harder-won than it was. The predicate is
// borrowed from the store rather than restated here so the two cannot drift.
//
// Conversations are derived from those same live messages for the same
// reason: an emptied conversation row is not a thread retrieval can return,
// and with --doc-key=conversation the thread count is the denominator a
// reader will reach for.
func (e *evaluator) collectCorpusStats(db *sql.DB) {
	if db == nil {
		return
	}
	live := store.LiveMessagesWhere("", true)
	_ = db.QueryRowContext(e.ctx,
		"SELECT COUNT(*) FROM messages WHERE "+live).Scan(&e.prov.Messages)
	_ = db.QueryRowContext(e.ctx,
		"SELECT COUNT(DISTINCT conversation_id) FROM messages WHERE "+live).Scan(&e.prov.Conversations)
}

// collidingDocKeyIDs returns doc ids that occur under more than one source in
// the live population, at most limit of them, in id order for stable output.
//
// It counts distinct source_id over the same live messages every search in this
// command draws from, so an id whose only other holder is dedup-hidden or
// deleted from its source is correctly not a collision: neither copy can be
// retrieved, so neither can be scored.
func collidingDocKeyIDs(ctx context.Context, db *sql.DB, col evalIDColumn, limit int) ([]string, error) {
	// The id may be NULL (no id assigned) or empty; eval.DedupeKeys drops both
	// from a ranking, so neither can collide with anything and both are
	// excluded here for the same reason.
	expr := col.expr()
	q := fmt.Sprintf(`
		SELECT %s
		FROM messages m
		%s
		WHERE %s AND %s IS NOT NULL AND %s <> ''
		GROUP BY %s
		HAVING COUNT(DISTINCT m.source_id) > 1
		ORDER BY %s
		LIMIT %d`,
		expr, col.join, store.LiveMessagesWhere("m", true), expr, expr, expr, expr, limit)

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// requireDisjointSourceIDs refuses a run whose archive cannot give the chosen
// --doc-key an unambiguous doc-id space.
//
// A qrels file is flat. "<qid> <iter> <docid> <rel>" has nowhere to record
// which connected account <docid> belongs to, and neither a TREC-derived
// collection nor judgments written by hand against a personal archive carry
// one. But both ids this command can key on are assigned by the *source* —
// source_message_id by the provider or the sending mail system,
// source_conversation_id by the provider's threading — and are unique only
// within it. msgvault is a multi-source archiver, so one archive routinely
// holds several accounts, and two of them can issue the same id for unrelated
// documents (two chat accounts each numbering their first conversation "1") or
// for related ones (the same mail delivered to two mailboxes). Either way the
// eval folds two documents into one key: a hit from an unjudged account
// inherits a judged account's relevance, or two genuinely distinct documents
// collapse and the ranking quietly loses a rank. Both move the score, both move
// it upward, and neither appears anywhere in the output — which is exactly the
// class of silent corruption this command exists to expose in other people's
// indexes.
//
// The fix is a precondition rather than a new key shape. Composing the source
// id into the key — as query.EntryKeyFacts.EntryKey does for explore entries,
// production's own answer to the same uniqueness problem — would make the key
// sound, but it would also change the shape of every doc id this command
// matches on, so every qrels file already written would stop matching. And it
// would stop matching by scoring a flat zero rather than by failing, which is
// the same silent corruption one level up.
//
// The precondition is disjointness, not single-source. An archive holding a
// Gmail account and a WhatsApp account has two sources and no overlapping ids
// at all; refusing to score it would be a wall built for a hazard that is not
// there. What has to hold is that the id space the qrels address is
// unambiguous, and "no id in it names documents in two sources" is exactly
// that. It is a property of the archive rather than of what a particular topic
// happened to retrieve, so it is established once, up front, instead of
// inferred from hits that may simply have got lucky — and it is established
// before the vector path is opened, so a run that cannot be scored does not
// first pay for an index and an embedding client.
func requireDisjointSourceIDs(ctx context.Context, db *sql.DB, docKey string, spec docKeySpec) error {
	if spec.idColumn.column == "" {
		// A registered key whose ids do not come from an archive column cannot
		// be checked here, and passing it silently would put the collision
		// straight back. Fail naming the key, so adding a doc-key forces an
		// answer to the question rather than allowing it to be skipped.
		return fmt.Errorf("--doc-key %q has no archive column to check for cross-source id collisions; "+
			"a doc-key whose ids come from elsewhere has to establish its own single-id-space guarantee", docKey)
	}
	// A single connected source cannot collide with itself, and that is the
	// common archive shape, so a cheap distinct-source count (backed by
	// idx_messages_source) skips the GROUP BY/HAVING scan — and its join, for
	// --doc-key=conversation — entirely for the run that does not need it.
	var sources int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT source_id) FROM messages WHERE "+store.LiveMessagesWhere("", true),
	).Scan(&sources); err != nil {
		return fmt.Errorf("count connected sources: %w", err)
	}
	if sources <= 1 {
		return nil
	}

	// Enough ids to make the error concrete without pasting an entire
	// re-imported mailbox into a terminal.
	const show = 10
	ids, err := collidingDocKeyIDs(ctx, db, spec.idColumn, show+1)
	if err != nil {
		return fmt.Errorf("check %s for cross-source id collisions: %w", spec.idColumn.column, err)
	}
	if len(ids) == 0 {
		return nil
	}
	count := strconv.Itoa(len(ids))
	if len(ids) > show {
		count = fmt.Sprintf("more than %d", show)
	}
	return fmt.Errorf("%s document ids in this archive (%s) belong to more than one connected source, "+
		"so --doc-key=%s cannot name a single document: %s is unique only within the source that "+
		"assigned it, while a qrels doc id records no source at all. Scoring this archive would fold "+
		"those sources' hits into one key and let an unjudged account's message inherit a judged one's "+
		"relevance. Evaluate an archive whose accounts do not share ids, or key the run on the other "+
		"--doc-key if its id space is disjoint",
		count, eval.FormatIDList(ids, show), docKey, spec.idColumn.column)
}

// collectVectorStats records the embedding model, fusion parameters and index
// size in force for this run, so a score can never be read without knowing
// what produced it.
func (e *evaluator) collectVectorStats(
	mainDB *sql.DB, vecCfg vector.Config, backend *sqlitevec.Backend, vecDBPath string, activeGen vector.GenerationID,
) {
	e.prov.VectorEnabled = true
	e.prov.EmbeddingModel = vecCfg.Embeddings.Model
	e.prov.APIFormat = string(vecCfg.Embeddings.EffectiveAPIFormat())
	e.prov.Dimension = vecCfg.Embeddings.Dimension
	e.prov.Endpoint = vecCfg.Embeddings.Endpoint
	e.prov.Backend = vecCfg.Backend
	e.prov.Fingerprint = vecCfg.GenerationFingerprint()
	e.prov.RRFK = vecCfg.Search.RRFK
	e.prov.KPerSignal = vecCfg.Search.KPerSignal
	e.prov.SubjectBoost = vecCfg.Search.SubjectBoost
	e.prov.IndexPath = vecDBPath
	// The pool ceiling is what a saturation diagnostic has to name to be
	// actionable, so the diagnostics carry it too.
	e.diag.kPerSignal = vecCfg.Search.KPerSignal

	if fi, err := os.Stat(vecDBPath); err == nil {
		e.prov.IndexSizeBytes = fi.Size()
	}
	// Backend.DB() is the backend's own accessor for exactly this kind of
	// read-only query, so the row count goes through it rather than opening a
	// second connection to the same file.
	//
	// Scope the count to the generation search reads. vectors.db keeps a
	// retired generation's rows — vec0 partition-key isolation means retiring
	// does not delete them — and a half-finished rebuild sits in the same
	// table, so COUNT(*) over the whole table describes the file on disk, not
	// the index this run queried. IndexSizeBytes already reports the file;
	// this number has to report the index.
	if vdb := backend.DB(); vdb != nil {
		_ = vdb.QueryRowContext(e.ctx,
			"SELECT COUNT(*) FROM embeddings WHERE generation_id = ?", int64(activeGen)).
			Scan(&e.prov.IndexedVectors)
		e.collectVectorCorpusStats(mainDB, vdb, vecCfg, activeGen)
	}
}

// collectVectorCorpusStats records the live population an account-scoped
// vector generation actually searches, when [vector.embed.scope] narrows it
// below the archive-wide Messages/Conversations collectCorpusStats already
// recorded.
//
// This deliberately does NOT require messages.embed_gen = gen, unlike
// Backend.EmbeddedMessageCount (production's own coverage accessor, built for
// a different question: "is this message's CURRENT content embedded"). A
// content change resets embed_gen to mark a message as needing re-embedding,
// but Backend.Search reads vectors.db purely by generation_id — it returns a
// message's stale vector until the re-embed actually runs, embed_gen or not.
// Requiring the stamp here would undercount relative to what a run can
// actually retrieve, the same "corpus" mismatch this field exists to fix in
// the other direction. So membership is: present in vectors.db for this
// generation, live, and in scope — exactly what Search can return, nothing
// narrower.
//
// Both counts are read from one query — the embedded message ids come from
// vectors.db, same as IndexedVectors reads, intersected once against
// main.db's live, scoped population — rather than a separate call per count,
// so a transient failure can only leave both at zero together, never one
// populated and the other not.
//
// Failures degrade the report rather than the run, same policy as
// collectCorpusStats: provenance is a courtesy to the reader, not a
// precondition for scoring.
func (e *evaluator) collectVectorCorpusStats(
	mainDB *sql.DB, vdb *sql.DB, vecCfg vector.Config, gen vector.GenerationID,
) {
	if mainDB == nil || vdb == nil {
		return
	}

	rows, err := vdb.QueryContext(e.ctx,
		`SELECT DISTINCT message_id FROM embeddings WHERE generation_id = ?`, int64(gen))
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return
	}

	blob, err := json.Marshal(ids)
	if err != nil {
		return
	}
	where := `id IN (SELECT value FROM json_each(?))
		AND ` + store.LiveMessagesWhere("", true)
	args := []any{string(blob)}
	if len(vecCfg.Embed.Scope.MessageTypes) > 0 {
		placeholders := make([]string, len(vecCfg.Embed.Scope.MessageTypes))
		for i, typ := range vecCfg.Embed.Scope.MessageTypes {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		where += fmt.Sprintf(" AND message_type IN (%s)", strings.Join(placeholders, ","))
	}
	if len(vecCfg.Embed.Scope.SourceIDs) > 0 {
		placeholders := make([]string, len(vecCfg.Embed.Scope.SourceIDs))
		for i, id := range vecCfg.Embed.Scope.SourceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where += fmt.Sprintf(" AND source_id IN (%s)", strings.Join(placeholders, ","))
	}
	// One row, one query: reading both counts off the same scan means a
	// transient failure here can only leave both fields at their zero value
	// together, never one populated and the other not — a partial success
	// would print a self-contradictory line no error report explains.
	_ = mainDB.QueryRowContext(e.ctx,
		"SELECT COUNT(DISTINCT id), COUNT(DISTINCT conversation_id) FROM messages WHERE "+where, args...).
		Scan(&e.prov.VectorMessages, &e.prov.VectorConversations)
}

// sortedCategories returns the category labels seen in a run, sorted for
// stable output.
func sortedCategories(catCounts map[string]int) []string {
	cats := make([]string, 0, len(catCounts))
	for c := range catCounts {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	return cats
}

// evalReport is everything one run produced, ready to render.
type evalReport struct {
	modes     []string
	aggs      map[string]*eval.Aggregate
	lats      map[string]*eval.LatencyTracker
	catAggs   map[string]map[string]*eval.Aggregate
	catCounts map[string]int
	prov      eval.RunConfig
	topics    int
	cutoffs   eval.Cutoffs
	diag      *runDiagnostics
}

// metricHeaders names the metric columns at the depths this run actually used,
// so a clamped cutoff can never be read as the standard one.
//
// MAP and MRR are qualified too. They take no cutoff, but the ranking handed to
// them is truncated to -n, so a relevant document below that rank is invisible
// to them exactly as it is to R@n: what the run measured is MAP@n and MRR@n.
// Printing them bare would offer them for comparison against a run that
// retrieved deeper, which is the same mislabeling the clamped headers exist to
// prevent. If the depth is somehow unknown there is nothing to qualify them
// with, so they stay bare rather than claiming a depth of zero.
func (r evalReport) metricHeaders() (p, ndcg, recall, mapAt, mrr string) {
	mapAt, mrr = "MAP", "MRR"
	if r.cutoffs.Depth > 0 {
		mapAt = fmt.Sprintf("MAP@%d", r.cutoffs.Depth)
		mrr = fmt.Sprintf("MRR@%d", r.cutoffs.Depth)
	}
	return fmt.Sprintf("P@%d", r.cutoffs.P),
		fmt.Sprintf("nDCG@%d", r.cutoffs.NDCG),
		fmt.Sprintf("R@%d", r.cutoffs.Recall),
		mapAt, mrr
}

func (r evalReport) table() {
	fmt.Printf("Evaluated %d topics (doc-key=%s, n=%d)\n", r.topics, evalDocKey, evalLimit)
	if !r.cutoffs.IsStandard() {
		fmt.Printf("Metric depths are clamped to -n: the standard P@%d/nDCG@%d/R@%d need -n %d or more.\n",
			eval.StandardCutoffs.P, eval.StandardCutoffs.NDCG, eval.StandardCutoffs.Recall,
			eval.StandardCutoffs.Recall)
	}

	// Provenance first: a score is not interpretable without it.
	fmt.Printf("\nRun configuration\n")
	pw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(pw, "  topics\t%s\n", r.prov.TopicsPath)
	_, _ = fmt.Fprintf(pw, "  qrels\t%s\n", r.prov.QrelsPath)
	_, _ = fmt.Fprintf(pw, "  corpus\t%d live messages, %d conversations\n",
		r.prov.Messages, r.prov.Conversations)
	if r.prov.VectorEnabled {
		if r.prov.VectorMessages != r.prov.Messages || r.prov.VectorConversations != r.prov.Conversations {
			_, _ = fmt.Fprintf(pw, "  vector corpus\t%d live messages, %d conversations "+
				"(embed.scope narrows this generation below the archive)\n",
				r.prov.VectorMessages, r.prov.VectorConversations)
		}
		_, _ = fmt.Fprintf(pw, "  embedding model\t%s (dim %d)\n", r.prov.EmbeddingModel, r.prov.Dimension)
		_, _ = fmt.Fprintf(pw, "  embedding api format\t%s\n", r.prov.APIFormat)
		_, _ = fmt.Fprintf(pw, "  embedding endpoint\t%s\n", r.prov.Endpoint)
		_, _ = fmt.Fprintf(pw, "  vector backend\t%s\n", r.prov.Backend)
		_, _ = fmt.Fprintf(pw, "  generation fingerprint\t%s\n", r.prov.Fingerprint)
		_, _ = fmt.Fprintf(pw, "  fusion\trrf_k=%d k_per_signal=%d subject_boost=%.2f\n",
			r.prov.RRFK, r.prov.KPerSignal, r.prov.SubjectBoost)
		_, _ = fmt.Fprintf(pw, "  vector index\t%d vectors in the active generation, %s on disk (%s)\n",
			r.prov.IndexedVectors, formatSize(r.prov.IndexSizeBytes), r.prov.IndexPath)
	} else {
		_, _ = fmt.Fprintf(pw, "  vector index\t(not used; --modes fts only)\n")
	}
	_ = pw.Flush()

	pCol, ndcgCol, rCol, mapCol, mrrCol := r.metricHeaders()
	fmt.Printf("\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// "topics" is per mode, not per run: a mode that cannot answer some topic
	// (a filter-only query has nothing to embed) scores fewer of them, and the
	// means are only comparable if the denominators are visible.
	header := []string{"MODE", "topics", pCol, ndcgCol, rCol, mapCol, mrrCol, "med ms", "p95 ms"}
	_, _ = fmt.Fprintln(w, strings.Join(header, "\t"))
	rule := make([]string, len(header))
	for i, h := range header {
		rule[i] = strings.Repeat("─", len([]rune(h)))
	}
	_, _ = fmt.Fprintln(w, strings.Join(rule, "\t"))
	for _, m := range r.modes {
		s := r.aggs[m].Mean()
		l := r.lats[m].Summary()
		_, _ = fmt.Fprintf(w, "%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.1f\t%.1f\n",
			m, r.aggs[m].N, s.P, s.NDCG, s.Recall, s.MAP, s.MRR, l.MedianMS, l.P95MS)
	}
	_ = w.Flush()

	// Per-category breakdown, only when the topics file carries labels.
	// Latency is tracked per mode, not per category, so those columns are
	// omitted here.
	if len(r.catCounts) > 0 {
		fmt.Printf("\nBy query category\n")
		cw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(cw, "MODE\tCATEGORY\ttopics\t%s\t%s\t%s\t%s\t%s\n",
			pCol, ndcgCol, rCol, mapCol, mrrCol)
		for _, m := range r.modes {
			for _, c := range sortedCategories(r.catCounts) {
				agg := r.catAggs[m][c]
				if agg == nil {
					continue
				}
				s := agg.Mean()
				_, _ = fmt.Fprintf(cw, "%s\t%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\n",
					m, c, agg.N, s.P, s.NDCG, s.Recall, s.MAP, s.MRR)
			}
		}
		_ = cw.Flush()
	}

	if notes := r.diag.notes(); len(notes) > 0 {
		fmt.Printf("\nDiagnostics\n")
		for _, n := range notes {
			fmt.Printf("  - %s\n", n)
		}
	}
}

func (r evalReport) json() error {
	pCol, ndcgCol, rCol, mapCol, mrrCol := r.metricHeaders()
	metricsOf := func(a *eval.Aggregate) map[string]any {
		s := a.Mean()
		return map[string]any{
			pCol: s.P, ndcgCol: s.NDCG, rCol: s.Recall, mapCol: s.MAP, mrrCol: s.MRR,
		}
	}
	results := make(map[string]any, len(r.modes))
	for _, m := range r.modes {
		entry := metricsOf(r.aggs[m])
		entry["topics"] = r.aggs[m].N
		entry["latency"] = r.lats[m].Summary()
		if len(r.catAggs[m]) > 0 {
			byCat := make(map[string]any, len(r.catAggs[m]))
			for c, a := range r.catAggs[m] {
				cm := metricsOf(a)
				cm["topics"] = a.N
				byCat[c] = cm
			}
			entry["by_category"] = byCat
		}
		results[m] = entry
	}
	out := map[string]any{
		"topics_evaluated": r.topics,
		"doc_key":          evalDocKey,
		"limit":            evalLimit,
		// One entry per metric, including the two whose depth is the retrieval
		// depth rather than a cutoff of their own, so a consumer can read every
		// metric's depth the same way instead of knowing which are special.
		"cutoffs": map[string]int{
			"precision": r.cutoffs.P, "ndcg": r.cutoffs.NDCG, "recall": r.cutoffs.Recall,
			"map": r.cutoffs.Depth, "mrr": r.cutoffs.Depth,
		},
		"modes":       r.modes,
		"run_config":  r.prov,
		"results":     results,
		"diagnostics": r.diag,
	}
	if len(r.catCounts) > 0 {
		out["topic_categories"] = r.catCounts
	}
	return printJSON(out)
}
