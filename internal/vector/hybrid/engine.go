package hybrid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
)

// Mode selects which signal(s) the engine runs.
type Mode string

const (
	// ModeFTS is the legacy FTS-only path. The engine rejects it because
	// the existing search path handles FTS directly.
	ModeFTS Mode = "fts"
	// ModeVector runs pure ANN search against the active generation.
	ModeVector Mode = "vector"
	// ModeHybrid runs fused BM25 + ANN via the FusingBackend capability.
	ModeHybrid Mode = "hybrid"
)

// SearchRequest is the caller-facing input to Engine.Search.
type SearchRequest struct {
	Mode         Mode
	FreeText     string
	FTSQuery     string // optional override; defaults to FreeText
	Filter       vector.Filter
	Limit        int
	SubjectTerms []string // lowercased terms for subject-boost check
	Explain      bool     // reserved for future use; no-op in this task
}

// ResultMeta returns engine-level metadata alongside the hit list.
type ResultMeta struct {
	Generation    vector.Generation
	PoolSaturated bool
	ReturnedCount int
	// QueryVector is the embedding of FreeText used for this search.
	// Callers use it to score within-message chunks without re-embedding.
	QueryVector []float32
}

// EmbeddingClient embeds free-text queries. The engine uses it once per
// Search call.
type EmbeddingClient interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// Config captures engine tuning knobs.
type Config struct {
	// ExpectedFingerprint is the "model:dimension" string the engine
	// checks against the active generation. If empty, the check is
	// skipped.
	ExpectedFingerprint string
	RRFK                int
	KPerSignal          int
	SubjectBoost        float64
	// Rebind converts ? placeholders to the driver's native form for the
	// participant/label lookup SQL that BuildFilter runs against mainDB.
	// Pass PostgreSQLDialect.Rebind on PG (pgx rejects bare ?); leave nil
	// (or SQLiteDialect.Rebind, which is identity) on SQLite.
	Rebind     func(string) string
	BuildScope vector.BuildScope
}

// Engine orchestrates the generation check, query embedding, and fusion
// call for vector/hybrid search requests.
type Engine struct {
	backend vector.Backend
	mainDB  *sql.DB
	client  EmbeddingClient
	cfg     Config
}

// NewEngine wires a backend, main DB handle, embedding client, and
// configuration into an Engine.
func NewEngine(backend vector.Backend, mainDB *sql.DB, client EmbeddingClient, cfg Config) *Engine {
	return &Engine{backend: backend, mainDB: mainDB, client: client, cfg: cfg}
}

// EmbedQuery embeds free text for within-message chunk scoring.
func (e *Engine) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty query")
	}
	vecs, err := e.client.Embed(ctx, []string{text})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("embed query: %w: %w", vector.ErrEmbeddingTimeout, err)
		}
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors, want 1", len(vecs))
	}
	return vecs[0], nil
}

// BuildFilter resolves a parsed Gmail-syntax query into a vector.Filter
// against the engine's main DB. Convenience wrapper around the
// package-level BuildFilter so callers that already hold an *Engine
// don't need to plumb a *sql.DB separately.
func (e *Engine) BuildFilter(ctx context.Context, q *search.Query) (vector.Filter, error) {
	return BuildFilter(ctx, e.mainDB, e.cfg.Rebind, q)
}

// Search runs hybrid or vector mode. Resolves the active generation
// via vector.ResolveActiveForFingerprint, so callers get the full
// family of sentinel errors:
//
//   - ErrIndexStale: an active generation exists but its fingerprint
//     differs from the configured model+dimension.
//   - ErrIndexBuilding: no active yet, but a build is in progress.
//   - ErrNotEnabled: no generation at all (vector search unused).
//
// mode=fts is rejected with a clear error (legacy path handles it).
func (e *Engine) Search(ctx context.Context, req SearchRequest) ([]vector.FusedHit, ResultMeta, error) {
	if req.Mode == ModeFTS {
		return nil, ResultMeta{}, errors.New("mode=fts should be handled by the legacy engine")
	}
	if req.Mode != ModeVector && req.Mode != ModeHybrid {
		return nil, ResultMeta{}, fmt.Errorf("unknown mode %q", req.Mode)
	}

	active, err := vector.ResolveActiveForFingerprint(ctx, e.backend, e.cfg.ExpectedFingerprint)
	if err != nil {
		return nil, ResultMeta{}, err
	}
	if err := e.validateBuildScope(req.Filter); err != nil {
		return nil, ResultMeta{}, err
	}

	if req.FreeText == "" {
		return nil, ResultMeta{}, errors.New("empty query")
	}

	vecs, err := e.client.Embed(ctx, []string{req.FreeText})
	if err != nil {
		// Surface deadline-exceeded distinctly so HTTP/MCP can map it
		// to a transient 503 instead of a generic 500. The handler
		// timeout (default 60s) often fires before a cold local
		// embedding endpoint responds, and "deadline exceeded" wrapped
		// inside an opaque error gives clients no way to know whether
		// to retry.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ResultMeta{}, fmt.Errorf("embed query: %w: %w", vector.ErrEmbeddingTimeout, err)
		}
		return nil, ResultMeta{}, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, ResultMeta{}, fmt.Errorf("embedder returned %d vectors, want 1", len(vecs))
	}
	queryVec := vecs[0]

	if req.Mode == ModeVector {
		hits, err := e.backend.Search(ctx, active.ID, queryVec, req.Limit, req.Filter)
		if err != nil {
			return nil, ResultMeta{}, fmt.Errorf("vector search: %w", err)
		}
		fused := vectorHitsToFused(hits)
		return fused, ResultMeta{
			Generation:    active,
			ReturnedCount: len(fused),
			PoolSaturated: len(fused) >= req.Limit,
			QueryVector:   queryVec,
		}, nil
	}

	// ModeHybrid: prefer FusingBackend.
	fb, ok := e.backend.(vector.FusingBackend)
	if !ok {
		return nil, ResultMeta{}, errors.New("hybrid mode requires a FusingBackend; non-fusing fallback not wired in MVP")
	}
	// FusedRequest.FTSTerms carries dialect-neutral, already-tokenized
	// and punctuation-filtered terms (see vector.FusedRequest); each
	// backend renders them through its own query dialect's BuildFTSTerm,
	// so PG and SQLite prefix-match the SAME term set instead of one
	// backend consuming the other's pre-built FTS5 expression. We
	// tokenize FreeText here (strings.Fields + drop punctuation-only
	// terms the FTS5 tokenizer would discard); an empty result skips the
	// BM25 leg (vector-only) rather than dispatching a malformed query.
	// An explicit FTSQuery override is also tokenized, never passed
	// through verbatim. Tokenizing here is what neutralizes FTS5/tsquery
	// metacharacters in a natural-language query (",", "?", ...) before
	// they reach either backend's parser (issue #366). Only the hybrid
	// path needs this: --mode fts sanitizes via Store.SearchMessages and
	// --mode vector has no BM25 branch at all.
	terms := ftsTerms(req.FreeText)
	if req.FTSQuery != "" {
		terms = ftsTerms(req.FTSQuery)
	}
	fReq := vector.FusedRequest{
		FTSTerms:     terms,
		QueryVec:     queryVec,
		Generation:   active.ID,
		KPerSignal:   e.cfg.KPerSignal,
		Limit:        req.Limit,
		RRFK:         e.cfg.RRFK,
		SubjectBoost: e.cfg.SubjectBoost,
		SubjectTerms: req.SubjectTerms,
		Filter:       req.Filter,
	}
	hits, saturated, err := fb.FusedSearch(ctx, fReq)
	if err != nil {
		return nil, ResultMeta{}, fmt.Errorf("fused search: %w", err)
	}
	return hits, ResultMeta{
		Generation:    active,
		ReturnedCount: len(hits),
		PoolSaturated: saturated,
		QueryVector:   queryVec,
	}, nil
}

func (e *Engine) validateBuildScope(filter vector.Filter) error {
	return ValidateBuildScope(e.cfg.BuildScope, filter)
}

// ValidateBuildScope rejects filters that cannot safely answer from a scoped
// embedding index. A non-empty build scope only covers those message types, so
// callers must make the query scope explicit and compatible before running ANN.
func ValidateBuildScope(buildScope vector.BuildScope, filter vector.Filter) error {
	scope := vector.NewBuildScope(buildScope.MessageTypes)
	if scope.IsEmpty() {
		return nil
	}
	if len(filter.MessageTypes) == 0 {
		return fmt.Errorf("%w: index is scoped to message_type=%s; add a matching message_type filter",
			vector.ErrIndexScopeMismatch, strings.Join(scope.MessageTypes, ","))
	}
	if !scope.AllowsMessageTypes(filter.MessageTypes) {
		return fmt.Errorf("%w: index is scoped to message_type=%s, query requested message_type=%s",
			vector.ErrIndexScopeMismatch,
			strings.Join(scope.MessageTypes, ","),
			strings.Join(vector.NewBuildScope(filter.MessageTypes).MessageTypes, ","))
	}
	return nil
}

// vectorHitsToFused wraps pure-vector hits in the FusedHit schema.
// BM25Score and RRFScore are both set to math.NaN(): "not present in
// this signal." Pure vector mode never applies Reciprocal Rank Fusion
// (there's only one signal to fuse), so reporting an RRF score would
// be a lie. Renderers and explain output already treat NaN as "skip
// this column," so the breakdown will show vector_score only.
func vectorHitsToFused(hits []vector.Hit) []vector.FusedHit {
	out := make([]vector.FusedHit, len(hits))
	for i, h := range hits {
		out[i] = vector.FusedHit{
			MessageID:   h.MessageID,
			BM25Score:   math.NaN(),
			VectorScore: h.Score,
			RRFScore:    math.NaN(),
		}
	}
	return out
}

// ftsTerms turns a raw free-text query into the dialect-neutral term
// slice for the hybrid BM25 branch, mirroring the --mode fts path
// (Store.SearchMessages → strings.Fields): each whitespace-separated
// term is kept verbatim, except terms the FTS5/tsquery tokenizers would
// drop entirely (punctuation-only) are skipped. Returns nil when
// nothing usable remains, which the fused query treats as "skip BM25"
// (vector-only) rather than dispatching a malformed query. Each backend
// renders these raw terms through its own query dialect's BuildFTSTerm,
// which quote-escapes / lexeme-splits them — that is what neutralizes
// embedded metacharacters like "," and "?" per backend (#366).
func ftsTerms(freeText string) []string {
	terms := strings.Fields(freeText)
	kept := terms[:0]
	for _, t := range terms {
		if hasFTSToken(t) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// hasFTSToken reports whether s contains a rune the default FTS5
// tokenizer (unicode61) emits as part of a token — a Unicode letter or
// digit. Punctuation-only terms tokenize to nothing, so a MATCH built
// from them is a syntax error; callers drop them. Mirrors the helper of
// the same name in internal/store (the two packages keep parallel
// minimal abstractions rather than share a dependency).
func hasFTSToken(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
