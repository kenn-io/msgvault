package hybrid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

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

// BuildFilter resolves a parsed Gmail-syntax query into a vector.Filter
// against the engine's main DB. Convenience wrapper around the
// package-level BuildFilter so callers that already hold an *Engine
// don't need to plumb a *sql.DB separately.
func (e *Engine) BuildFilter(ctx context.Context, q *search.Query) (vector.Filter, error) {
	return BuildFilter(ctx, e.mainDB, q)
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
		return nil, ResultMeta{}, fmt.Errorf("mode=fts should be handled by the legacy engine")
	}
	if req.Mode != ModeVector && req.Mode != ModeHybrid {
		return nil, ResultMeta{}, fmt.Errorf("unknown mode %q", req.Mode)
	}

	active, err := vector.ResolveActiveForFingerprint(ctx, e.backend, e.cfg.ExpectedFingerprint)
	if err != nil {
		return nil, ResultMeta{}, err
	}

	if req.FreeText == "" {
		return nil, ResultMeta{}, fmt.Errorf("empty query")
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
		}, nil
	}

	// ModeHybrid: prefer FusingBackend.
	fb, ok := e.backend.(vector.FusingBackend)
	if !ok {
		return nil, ResultMeta{}, errors.New("hybrid mode requires a FusingBackend; non-fusing fallback not wired in MVP")
	}
	fReq := vector.FusedRequest{
		FTSQuery:     firstNonEmpty(req.FTSQuery, req.FreeText),
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
	}, nil
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
