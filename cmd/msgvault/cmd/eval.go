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
is the conversation, a thread is counted once, at its best-ranked message.

Every run reports the embedding model, index settings and index size that
produced it, plus per-query latency, because a quality number is not
comparable — or even interpretable — without them.

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
	evalCmd.Flags().StringVar(&evalDocKey, "doc-key", "message", "Which id qrels reference: "+docKeyNames())
	evalCmd.Flags().IntVarP(&evalLimit, "limit", "n", 100, "Results retrieved per query")
	evalCmd.Flags().BoolVar(&evalJSON, flagJSON, false, "Output as JSON")
	_ = evalCmd.MarkFlagRequired("qrels")
	_ = evalCmd.MarkFlagRequired("topics")
}

// docKeyFunc extracts, from a retrieved hit, the stable document id that
// qrels judge against. It is the only place a doc-key's meaning lives: the
// scoring core (eval.Evaluate, eval.Aggregate, eval.DedupeKeys) operates on
// the opaque string keys these return and never learns what they identify.
type docKeyFunc func(query.MessageSummary) string

// docKeyFuncs maps each --doc-key value to its id extractor. Adding a new
// judged unit — say a reconstructed-thread id resolved through an external
// message-id -> thread-id mapping file — means registering one more entry
// here (a closure over the loaded mapping); the CLI validation, the scoring
// core and the output paths all pick it up unchanged.
var docKeyFuncs = map[string]docKeyFunc{
	"message":      func(m query.MessageSummary) string { return m.SourceMessageID },
	"conversation": func(m query.MessageSummary) string { return m.SourceConversationID },
}

// docKeyNames renders the valid --doc-key values for usage and error text.
func docKeyNames() string {
	names := make([]string, 0, len(docKeyFuncs))
	for n := range docKeyFuncs {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// evaluator bundles the engines and config needed to turn a query string into
// a ranked list of document ids for a given search mode.
type evaluator struct {
	ctx   context.Context
	qeng  query.Engine
	heng  *hybrid.Engine
	keyOf docKeyFunc
	prov  eval.RunConfig
}

func (e *evaluator) rankedFTS(qstr string, limit int) ([]string, error) {
	res, err := e.qeng.Search(e.ctx, search.Parse(qstr), limit, 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res))
	for _, m := range res {
		out = append(out, e.keyOf(m))
	}
	return eval.DedupeKeys(out), nil
}

func (e *evaluator) rankedVector(mode, qstr string, limit int) ([]string, error) {
	q := search.Parse(qstr)
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
	hits, _, err := e.heng.Search(e.ctx, hybrid.SearchRequest{
		Mode:         hybrid.Mode(mode),
		FreeText:     strings.Join(q.TextTerms, " "),
		Filter:       filter,
		Limit:        limit,
		SubjectTerms: subjectTerms,
	})
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.MessageID
	}
	summaries, err := e.qeng.GetMessageSummariesByIDs(e.ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("map message ids: %w", err)
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, m := range summaries {
		byID[m.ID] = m
	}
	// Preserve the engine's ranking order; drop any hit that couldn't hydrate.
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if m, ok := byID[h.MessageID]; ok {
			out = append(out, e.keyOf(m))
		}
	}
	return eval.DedupeKeys(out), nil
}

func (e *evaluator) ranked(mode, qstr string, limit int) ([]string, error) {
	switch mode {
	case "fts":
		return e.rankedFTS(qstr, limit)
	case "vector", "hybrid":
		return e.rankedVector(mode, qstr, limit)
	default:
		return nil, fmt.Errorf("unknown mode %q (want fts|vector|hybrid)", mode)
	}
}

func runEval(cmd *cobra.Command, _ []string) error {
	keyOf, ok := docKeyFuncs[evalDocKey]
	if !ok {
		return usageErr(cmd, fmt.Errorf("invalid --doc-key %q (want %s)", evalDocKey, docKeyNames()))
	}
	modes, needVec, err := parseEvalModes(evalModes)
	if err != nil {
		return usageErr(cmd, err)
	}

	qrels, qrelsStats, err := eval.LoadQrels(evalQrels)
	if err != nil {
		return err
	}
	topics, topicsStats, err := eval.LoadTopics(evalTopics)
	if err != nil {
		return err
	}

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
		keyOf: keyOf,
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
		for _, m := range modes {
			start := time.Now()
			ranked, err := ev.ranked(m, t.Query, evalLimit)
			elapsed := time.Since(start)
			if err != nil {
				return fmt.Errorf("topic %s, mode %s: %w", t.ID, m, err)
			}
			lats[m].Add(elapsed)
			s := eval.Evaluate(ranked, rel)
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
		}
		if t.Category != "" {
			catCounts[t.Category]++
		}
		scored++
	}
	if scored == 0 {
		return fmt.Errorf("none of the %d topics had relevance judgments in %s "+
			"(qrels: %s; topics: %s); check that the qids in both files refer to the same queries",
			len(topics), evalQrels, qrelsStats, topicsStats)
	}

	if evalJSON {
		return outputEvalJSON(modes, aggs, lats, catAggs, catCounts, ev.prov, scored)
	}
	return outputEvalTable(modes, aggs, lats, catAggs, catCounts, ev.prov, scored)
}

// parseEvalModes splits and validates the --modes flag.
func parseEvalModes(spec string) (modes []string, needVec bool, err error) {
	for _, m := range strings.Split(spec, ",") {
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

func outputEvalTable(modes []string, aggs map[string]*eval.Aggregate, lats map[string]*eval.LatencyTracker,
	catAggs map[string]map[string]*eval.Aggregate, catCounts map[string]int, p eval.RunConfig, n int) error {
	fmt.Printf("Evaluated %d topics (doc-key=%s, n=%d)\n", n, evalDocKey, evalLimit)

	// Provenance first: a score is not interpretable without it.
	fmt.Printf("\nRun configuration\n")
	pw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(pw, "  topics\t%s\n", p.TopicsPath)
	_, _ = fmt.Fprintf(pw, "  qrels\t%s\n", p.QrelsPath)
	_, _ = fmt.Fprintf(pw, "  corpus\t%d messages, %d conversations\n", p.Messages, p.Conversations)
	if p.VectorEnabled {
		_, _ = fmt.Fprintf(pw, "  embedding model\t%s (dim %d)\n", p.EmbeddingModel, p.Dimension)
		_, _ = fmt.Fprintf(pw, "  embedding endpoint\t%s\n", p.Endpoint)
		_, _ = fmt.Fprintf(pw, "  vector backend\t%s\n", p.Backend)
		_, _ = fmt.Fprintf(pw, "  generation fingerprint\t%s\n", p.Fingerprint)
		_, _ = fmt.Fprintf(pw, "  fusion\trrf_k=%d k_per_signal=%d subject_boost=%.2f\n",
			p.RRFK, p.KPerSignal, p.SubjectBoost)
		_, _ = fmt.Fprintf(pw, "  vector index\t%d vectors, %s (%s)\n",
			p.IndexedVectors, formatSize(p.IndexSizeBytes), p.IndexPath)
	} else {
		_, _ = fmt.Fprintf(pw, "  vector index\t(not used; --modes fts only)\n")
	}
	_ = pw.Flush()

	fmt.Printf("\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "MODE\tP@10\tnDCG@10\tR@100\tMAP\tMRR\tmed ms\tp95 ms")
	_, _ = fmt.Fprintln(w, "────\t────\t───────\t─────\t───\t───\t──────\t──────")
	for _, m := range modes {
		s := aggs[m].Mean()
		l := lats[m].Summary()
		_, _ = fmt.Fprintf(w, "%s\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.1f\t%.1f\n",
			m, s.P10, s.NDCG10, s.R100, s.MAP, s.MRR, l.MedianMS, l.P95MS)
	}
	_ = w.Flush()

	// Per-category breakdown, only when the topics file carries labels.
	// Latency is tracked per mode, not per category, so those columns are
	// omitted here.
	if len(catCounts) > 0 {
		fmt.Printf("\nBy query category\n")
		cw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(cw, "MODE\tCATEGORY\ttopics\tP@10\tnDCG@10\tR@100\tMAP\tMRR")
		for _, m := range modes {
			for _, c := range sortedCategories(catCounts) {
				agg := catAggs[m][c]
				if agg == nil {
					continue
				}
				s := agg.Mean()
				_, _ = fmt.Fprintf(cw, "%s\t%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\n",
					m, c, agg.N, s.P10, s.NDCG10, s.R100, s.MAP, s.MRR)
			}
		}
		_ = cw.Flush()
	}
	return nil
}

func outputEvalJSON(modes []string, aggs map[string]*eval.Aggregate, lats map[string]*eval.LatencyTracker,
	catAggs map[string]map[string]*eval.Aggregate, catCounts map[string]int, p eval.RunConfig, n int) error {
	metricsOf := func(a *eval.Aggregate) map[string]any {
		s := a.Mean()
		return map[string]any{
			"P@10": s.P10, "nDCG@10": s.NDCG10, "R@100": s.R100, "MAP": s.MAP, "MRR": s.MRR,
		}
	}
	results := make(map[string]any, len(modes))
	for _, m := range modes {
		entry := metricsOf(aggs[m])
		entry["latency"] = lats[m].Summary()
		if len(catAggs[m]) > 0 {
			byCat := make(map[string]any, len(catAggs[m]))
			for c, a := range catAggs[m] {
				cm := metricsOf(a)
				cm["topics"] = a.N
				byCat[c] = cm
			}
			entry["by_category"] = byCat
		}
		results[m] = entry
	}
	out := map[string]any{
		"topics_evaluated": n,
		"doc_key":          evalDocKey,
		"limit":            evalLimit,
		"modes":            modes,
		"run_config":       p,
		"results":          results,
	}
	if len(catCounts) > 0 {
		out["topic_categories"] = catCounts
	}
	return printJSON(out)
}
