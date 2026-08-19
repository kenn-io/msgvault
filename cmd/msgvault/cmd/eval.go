//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
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
guessed.

Inputs (TREC-style):
  --qrels   judgments file, one per line: "<qid> <iter> <docid> <rel>"
            (rel >= 1 means relevant; the iter column is ignored)
  --topics  queries file, tab-separated: "<qid>\t<query text>[\t<category>]"
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

Metric depths follow -n: the standard P@10 / nDCG@10 / R@100 are reported when
the run retrieves at least that deep, and are clamped to -n below it (a run
that only ever looks 20 deep has no recall@100, and labelling one would invite
a false comparison). The column headers always name the depth actually used.

Every run reports the embedding model, index settings and index size that
produced it, plus per-query latency, because a quality number is not
comparable — or even interpretable — without them. Anything that went wrong
without being fatal — unparseable judgment lines, hits that could not be
hydrated from the archive, topics no mode could score — is reported under
"Diagnostics" rather than silently folded into the scores.

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

// docKeySpec describes one --doc-key value.
type docKeySpec struct {
	// extract pulls, from a retrieved hit, the stable document id that qrels
	// judge against. It is the only place a doc-key's meaning lives: the
	// scoring core (eval.Evaluate, eval.Aggregate, eval.DedupeKeys) operates
	// on the opaque string keys these return and never learns what they
	// identify.
	extract func(query.MessageSummary) string
	// collapses is true when the judged unit is coarser than the retrieved
	// one, so several hits routinely fold into a single key. Retrieval then
	// has to over-fetch messages to fill the requested depth with *distinct*
	// keys — see eval.OverFetchPlan and evaluator.rankedKeys.
	collapses bool
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
			extract: func(m query.MessageSummary) string { return m.SourceMessageID },
		},
		// Many messages share one conversation, so filling n distinct threads
		// takes more than n messages.
		"conversation": {
			extract:   func(m query.MessageSummary) string { return m.SourceConversationID },
			collapses: true,
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
	QrelsLoad       eval.LoadStats `json:"qrels_load"`
	TopicsLoad      eval.LoadStats `json:"topics_load"`
	UnhydratedHits  int            `json:"unhydrated_hits,omitempty"`
	DepthShortfalls int            `json:"depth_shortfalls,omitempty"`
	SkippedCells    []string       `json:"skipped_cells,omitempty"`
}

// skip records that one topic/mode combination could not be scored.
func (d *runDiagnostics) skip(topicID, mode, reason string) {
	d.SkippedCells = append(d.SkippedCells, fmt.Sprintf("topic %s / %s: %s", topicID, mode, reason))
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
	out = append(out, d.SkippedCells...)
	return out
}

// evaluator bundles the engines and config needed to turn a query string into
// a ranked list of document ids for a given search mode.
type evaluator struct {
	ctx   context.Context
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
		// The engine returned less than we asked for, so it has nothing left
		// to give and a deeper fetch would return the same list.
		exhausted := res.raw < n
		if filled || exhausted || i == len(plan)-1 {
			e.diag.UnhydratedHits += res.dropped
			if !filled && !exhausted {
				e.diag.DepthShortfalls++
			}
			return eval.TruncateKeys(deduped, e.limit), nil
		}
	}
	// Unreachable: OverFetchPlan is never empty and the loop always returns on
	// its final step.
	return nil, nil
}

func (e *evaluator) rankedFTS(qstr string) ([]string, error) {
	return e.rankedKeys(func(n int) (fetchResult, error) {
		res, err := e.qeng.Search(e.ctx, search.Parse(qstr), n, 0)
		if err != nil {
			return fetchResult{}, err
		}
		keys := make([]string, 0, len(res))
		for _, m := range res {
			keys = append(keys, e.key.extract(m))
		}
		return fetchResult{keys: keys, raw: len(res)}, nil
	})
}

func (e *evaluator) rankedVector(mode, qstr string) ([]string, error) {
	q := search.Parse(qstr)
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
		hits, _, err := e.heng.Search(e.ctx, hybrid.SearchRequest{
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
			return fetchResult{}, nil
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
		out := fetchResult{keys: make([]string, 0, len(hits)), raw: len(hits)}
		for _, h := range hits {
			m, ok := byID[h.MessageID]
			if !ok {
				out.dropped++
				continue
			}
			out.keys = append(out.keys, e.key.extract(m))
		}
		return out, nil
	})
}

func (e *evaluator) ranked(mode, qstr string) ([]string, error) {
	switch mode {
	case "fts":
		return e.rankedFTS(qstr)
	case "vector", "hybrid":
		return e.rankedVector(mode, qstr)
	default:
		return nil, fmt.Errorf("unknown mode %q (want fts|vector|hybrid)", mode)
	}
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

	ev := &evaluator{
		ctx:   ctx,
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
		rel := qrels.RelevantSet(t.ID)
		if len(rel) == 0 {
			continue // topic has no judgments in this qrels file
		}
		anyMode := false
		for _, m := range modes {
			start := time.Now()
			ranked, err := ev.ranked(m, t.Query)
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

// parseEvalModes splits and validates the --modes flag.
func parseEvalModes(spec string) (modes []string, needVec bool, err error) {
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
	if cfg.Vector.Embeddings.Endpoint == "" || cfg.Vector.Embeddings.Model == "" {
		return nil, errors.New("vector/hybrid modes need [vector.embeddings] endpoint and model in config")
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
	if _, err := vector.ResolveActiveForFingerprint(ctx, backend, vecCfg.GenerationFingerprint()); err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("resolve active generation: %w", err)
	}

	embedClient := embed.NewClient(embed.Config{
		Endpoint:   vecCfg.Embeddings.Endpoint,
		APIKey:     vecCfg.Embeddings.APIKey(),
		Model:      vecCfg.Embeddings.Model,
		Dimension:  vecCfg.Embeddings.Dimension,
		Timeout:    vecCfg.Embeddings.Timeout,
		MaxRetries: vecCfg.Embeddings.MaxRetries,
	})
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
	e.collectVectorStats(vecCfg, backend, vecDBPath)

	return func() { _ = backend.Close() }, nil
}

// collectCorpusStats records how big the searched archive is. It reads the
// store's own handle so it also works for --modes fts, which never opens the
// vector path. Failures are non-fatal: missing provenance should degrade the
// report, never abort a run.
func (e *evaluator) collectCorpusStats(db *sql.DB) {
	if db == nil {
		return
	}
	_ = db.QueryRowContext(e.ctx, "SELECT COUNT(*) FROM messages").Scan(&e.prov.Messages)
	_ = db.QueryRowContext(e.ctx, "SELECT COUNT(*) FROM conversations").Scan(&e.prov.Conversations)
}

// collectVectorStats records the embedding model, fusion parameters and index
// size in force for this run, so a score can never be read without knowing
// what produced it.
func (e *evaluator) collectVectorStats(vecCfg vector.Config, backend *sqlitevec.Backend, vecDBPath string) {
	e.prov.VectorEnabled = true
	e.prov.EmbeddingModel = vecCfg.Embeddings.Model
	e.prov.Dimension = vecCfg.Embeddings.Dimension
	e.prov.Endpoint = vecCfg.Embeddings.Endpoint
	e.prov.Backend = vecCfg.Backend
	e.prov.Fingerprint = vecCfg.GenerationFingerprint()
	e.prov.RRFK = vecCfg.Search.RRFK
	e.prov.KPerSignal = vecCfg.Search.KPerSignal
	e.prov.SubjectBoost = vecCfg.Search.SubjectBoost
	e.prov.IndexPath = vecDBPath

	if fi, err := os.Stat(vecDBPath); err == nil {
		e.prov.IndexSizeBytes = fi.Size()
	}
	// Backend.DB() is the backend's own accessor for exactly this kind of
	// read-only query, so the row count goes through it rather than opening a
	// second connection to the same file.
	if vdb := backend.DB(); vdb != nil {
		_ = vdb.QueryRowContext(e.ctx, "SELECT COUNT(*) FROM embeddings").Scan(&e.prov.IndexedVectors)
	}
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
func (r evalReport) metricHeaders() (p, ndcg, recall string) {
	return fmt.Sprintf("P@%d", r.cutoffs.P),
		fmt.Sprintf("nDCG@%d", r.cutoffs.NDCG),
		fmt.Sprintf("R@%d", r.cutoffs.Recall)
}

func (r evalReport) table() {
	fmt.Printf("Evaluated %d topics (doc-key=%s, n=%d)\n", r.topics, evalDocKey, evalLimit)
	if r.cutoffs != eval.StandardCutoffs {
		fmt.Printf("Metric depths are clamped to -n: the standard P@%d/nDCG@%d/R@%d need -n %d or more.\n",
			eval.StandardCutoffs.P, eval.StandardCutoffs.NDCG, eval.StandardCutoffs.Recall,
			eval.StandardCutoffs.Recall)
	}

	// Provenance first: a score is not interpretable without it.
	fmt.Printf("\nRun configuration\n")
	pw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(pw, "  topics\t%s\n", r.prov.TopicsPath)
	_, _ = fmt.Fprintf(pw, "  qrels\t%s\n", r.prov.QrelsPath)
	_, _ = fmt.Fprintf(pw, "  corpus\t%d messages, %d conversations\n", r.prov.Messages, r.prov.Conversations)
	if r.prov.VectorEnabled {
		_, _ = fmt.Fprintf(pw, "  embedding model\t%s (dim %d)\n", r.prov.EmbeddingModel, r.prov.Dimension)
		_, _ = fmt.Fprintf(pw, "  embedding endpoint\t%s\n", r.prov.Endpoint)
		_, _ = fmt.Fprintf(pw, "  vector backend\t%s\n", r.prov.Backend)
		_, _ = fmt.Fprintf(pw, "  generation fingerprint\t%s\n", r.prov.Fingerprint)
		_, _ = fmt.Fprintf(pw, "  fusion\trrf_k=%d k_per_signal=%d subject_boost=%.2f\n",
			r.prov.RRFK, r.prov.KPerSignal, r.prov.SubjectBoost)
		_, _ = fmt.Fprintf(pw, "  vector index\t%d vectors, %s (%s)\n",
			r.prov.IndexedVectors, formatSize(r.prov.IndexSizeBytes), r.prov.IndexPath)
	} else {
		_, _ = fmt.Fprintf(pw, "  vector index\t(not used; --modes fts only)\n")
	}
	_ = pw.Flush()

	pCol, ndcgCol, rCol := r.metricHeaders()
	fmt.Printf("\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// "topics" is per mode, not per run: a mode that cannot answer some topic
	// (a filter-only query has nothing to embed) scores fewer of them, and the
	// means are only comparable if the denominators are visible.
	header := []string{"MODE", "topics", pCol, ndcgCol, rCol, "MAP", "MRR", "med ms", "p95 ms"}
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
		_, _ = fmt.Fprintf(cw, "MODE\tCATEGORY\ttopics\t%s\t%s\t%s\tMAP\tMRR\n", pCol, ndcgCol, rCol)
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
	pCol, ndcgCol, rCol := r.metricHeaders()
	metricsOf := func(a *eval.Aggregate) map[string]any {
		s := a.Mean()
		return map[string]any{
			pCol: s.P, ndcgCol: s.NDCG, rCol: s.Recall, "MAP": s.MAP, "MRR": s.MRR,
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
		"cutoffs": map[string]int{
			"precision": r.cutoffs.P, "ndcg": r.cutoffs.NDCG, "recall": r.cutoffs.Recall,
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
