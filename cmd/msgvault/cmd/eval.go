//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/mattn/go-sqlite3"
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
  --topics  queries file, tab-separated: "<qid>\t<query text>"

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
	evalCmd.Flags().StringVar(&evalDocKey, "doc-key", "message", "Which id qrels reference: message|conversation")
	evalCmd.Flags().IntVarP(&evalLimit, "limit", "n", 100, "Results retrieved per query")
	evalCmd.Flags().BoolVar(&evalJSON, flagJSON, false, "Output as JSON")
	_ = evalCmd.MarkFlagRequired("qrels")
	_ = evalCmd.MarkFlagRequired("topics")
}

// evaluator bundles the engines and config needed to turn a query string into
// a ranked list of document ids for a given search mode.
type evaluator struct {
	ctx    context.Context
	qeng   query.Engine
	heng   *hybrid.Engine
	mainDB *sql.DB
	docKey string
	prov   eval.RunConfig
}

// keyOf selects the stable document id (the field qrels reference) for a hit.
func (e *evaluator) keyOf(m query.MessageSummary) string {
	if e.docKey == "conversation" {
		return m.SourceConversationID
	}
	return m.SourceMessageID
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
	if evalDocKey != "message" && evalDocKey != "conversation" {
		return usageErr(cmd, fmt.Errorf("invalid --doc-key %q (want message|conversation)", evalDocKey))
	}
	modes, needVec, err := parseEvalModes(evalModes)
	if err != nil {
		return usageErr(cmd, err)
	}

	qrels, err := eval.LoadQrels(evalQrels)
	if err != nil {
		return err
	}
	topics, err := eval.LoadTopics(evalTopics)
	if err != nil {
		return err
	}
	if len(topics) == 0 {
		return fmt.Errorf("no topics loaded from %s", evalTopics)
	}

	ctx := cmd.Context()

	// Store + query engine: serves FTS search and the rowid -> source-id
	// mapping for vector/hybrid hits. Opening it also runs the schema
	// migrations the vector backend's raw sql.DB relies on.
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
		ctx:    ctx,
		qeng:   query.NewEngine(s.DB(), s.IsPostgreSQL()),
		docKey: evalDocKey,
	}

	if needVec {
		cleanup, err := ev.attachVector(ctx)
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
			aggs[m].Add(eval.Evaluate(ranked, rel))
		}
		scored++
	}
	if scored == 0 {
		return fmt.Errorf("none of the %d topics had relevance judgments in %s", len(topics), evalQrels)
	}

	if evalJSON {
		return outputEvalJSON(modes, aggs, lats, ev.prov, scored)
	}
	return outputEvalTable(modes, aggs, lats, ev.prov, scored)
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
		return nil, false, fmt.Errorf("--modes is empty")
	}
	return modes, needVec, nil
}

// attachVector wires the sqlite-vec backend and hybrid engine onto the
// evaluator (mirroring the search command's vector path) and returns a
// cleanup closure that closes the resources it opened.
func (e *evaluator) attachVector(ctx context.Context) (func(), error) {
	if !cfg.Vector.Enabled {
		return nil, fmt.Errorf("vector/hybrid modes need [vector].enabled = true in config")
	}
	if cfg.Vector.Embeddings.Endpoint == "" || cfg.Vector.Embeddings.Model == "" {
		return nil, fmt.Errorf("vector/hybrid modes need [vector.embeddings] endpoint and model in config")
	}

	mainDB, err := sql.Open("sqlite3", cfg.DatabaseDSN())
	if err != nil {
		return nil, fmt.Errorf("open main db: %w", err)
	}

	vecDBPath := cfg.Vector.DBPath
	if vecDBPath == "" {
		vecDBPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	if err := sqlitevec.RegisterExtension(); err != nil {
		_ = mainDB.Close()
		return nil, fmt.Errorf("register sqlite-vec: %w", err)
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      vecDBPath,
		MainPath:  cfg.DatabaseDSN(),
		Dimension: cfg.Vector.Embeddings.Dimension,
		MainDB:    mainDB,
	})
	if err != nil {
		_ = mainDB.Close()
		return nil, fmt.Errorf("open vectors.db: %w", err)
	}
	if _, err := vector.ResolveActiveForFingerprint(ctx, backend, cfg.Vector.GenerationFingerprint()); err != nil {
		_ = backend.Close()
		_ = mainDB.Close()
		return nil, fmt.Errorf("resolve active generation: %w", err)
	}

	embedClient := embed.NewClient(embed.Config{
		Endpoint:   cfg.Vector.Embeddings.Endpoint,
		APIKey:     cfg.Vector.Embeddings.APIKey(),
		Model:      cfg.Vector.Embeddings.Model,
		Dimension:  cfg.Vector.Embeddings.Dimension,
		Timeout:    cfg.Vector.Embeddings.Timeout,
		MaxRetries: cfg.Vector.Embeddings.MaxRetries,
	})
	e.heng = hybrid.NewEngine(backend, mainDB, embedClient, hybrid.Config{
		ExpectedFingerprint: cfg.Vector.GenerationFingerprint(),
		RRFK:                cfg.Vector.Search.RRFK,
		KPerSignal:          cfg.Vector.Search.KPerSignal,
		SubjectBoost:        cfg.Vector.Search.SubjectBoost,
	})
	e.mainDB = mainDB
	e.collectVectorStats(vecDBPath)

	return func() {
		_ = backend.Close()
		_ = mainDB.Close()
	}, nil
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
func (e *evaluator) collectVectorStats(vecDBPath string) {
	e.prov.VectorEnabled = true
	e.prov.EmbeddingModel = cfg.Vector.Embeddings.Model
	e.prov.Dimension = cfg.Vector.Embeddings.Dimension
	e.prov.Endpoint = cfg.Vector.Embeddings.Endpoint
	e.prov.Backend = cfg.Vector.Backend
	e.prov.Fingerprint = cfg.Vector.GenerationFingerprint()
	e.prov.RRFK = cfg.Vector.Search.RRFK
	e.prov.KPerSignal = cfg.Vector.Search.KPerSignal
	e.prov.SubjectBoost = cfg.Vector.Search.SubjectBoost
	e.prov.IndexPath = vecDBPath

	if fi, err := os.Stat(vecDBPath); err == nil {
		e.prov.IndexSizeBytes = fi.Size()
	}
	// Read the row count from a short-lived read-only handle: the vector
	// backend owns its own connection and we must not disturb it.
	if vdb, err := sql.Open("sqlite3", vecDBPath+"?mode=ro"); err == nil {
		defer func() { _ = vdb.Close() }()
		_ = vdb.QueryRowContext(e.ctx, "SELECT COUNT(*) FROM embeddings").Scan(&e.prov.IndexedVectors)
	}
}

// humanBytes renders a byte count compactly for the table output.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func outputEvalTable(modes []string, aggs map[string]*eval.Aggregate, lats map[string]*eval.LatencyTracker, p eval.RunConfig, n int) error {
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
			p.IndexedVectors, humanBytes(p.IndexSizeBytes), p.IndexPath)
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
	return nil
}

func outputEvalJSON(modes []string, aggs map[string]*eval.Aggregate, lats map[string]*eval.LatencyTracker, p eval.RunConfig, n int) error {
	results := make(map[string]any, len(modes))
	for _, m := range modes {
		s := aggs[m].Mean()
		results[m] = map[string]any{
			"P@10": s.P10, "nDCG@10": s.NDCG10, "R@100": s.R100, "MAP": s.MAP, "MRR": s.MRR,
			"latency": lats[m].Summary(),
		}
	}
	return printJSON(map[string]any{
		"topics_evaluated": n,
		"doc_key":          evalDocKey,
		"limit":            evalLimit,
		"modes":            modes,
		"run_config":       p,
		"results":          results,
	})
}
