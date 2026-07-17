package vector

import (
	"context"
	"time"
)

// GenerationID identifies one index generation.
type GenerationID int64

// GenerationState is one of: building, active, retired.
type GenerationState string

const (
	GenerationBuilding GenerationState = "building"
	GenerationActive   GenerationState = "active"
	GenerationRetired  GenerationState = "retired"
)

// Generation describes an index generation — a complete corpus
// embedding under one model+dimension.
type Generation struct {
	ID        GenerationID
	Model     string
	Dimension int
	// Fingerprint is the opaque identifier supplied by the caller at
	// CreateGeneration time (typically Config.GenerationFingerprint(),
	// which folds the preprocessing policy into the model+dimension
	// pair). Callers compare equality only — do not parse it.
	Fingerprint  string
	State        GenerationState
	StartedAt    time.Time
	CompletedAt  *time.Time
	ActivatedAt  *time.Time
	MessageCount int64
}

// Chunk is a pre-computed embedding to persist in the index. A long
// message produces multiple chunks distinguished by ChunkIndex (0-based,
// dense, gap-free). Short messages produce exactly one chunk with
// ChunkIndex=0, which is the legacy single-vector behavior.
//
// Backends key vectors by (GenerationID, MessageID, ChunkIndex). Search
// returns at most one Hit per MessageID; if multiple chunks of the same
// message match, the backend keeps the best-scoring chunk and discards
// the rest. ChunkCharStart/ChunkCharEnd are 0-based offsets into the
// preprocessed text and are stored for debugging only — search results
// do not currently surface "which chunk matched".
type Chunk struct {
	MessageID      int64
	ChunkIndex     int
	Vector         []float32
	SourceCharLen  int
	ChunkCharStart int
	ChunkCharEnd   int
	Truncated      bool
}

// Filter carries the structured filters pushed into both signal CTEs
// in hybrid search. Values are pre-resolved to IDs at the Go layer
// (addresses → participant IDs, labels → label IDs) so backend code
// only deals in integers.
//
// Semantics match the existing SQLite search path (internal/store/api.go,
// internal/query/sqlite.go): each repeated same-field operator becomes
// its own AND'd EXISTS clause at the message level, so a query like
// `from:alice from:bob` requires the message to have one `from`
// participant matching "alice" AND another (or the same) matching
// "bob". Within a group, IDs are OR'd (any matching participant
// satisfies the group); across groups they are AND'd.
//
//   - Sender/To/Cc/Bcc/LabelGroups are AND-of-OR groups: each inner
//     slice is one search-token resolution (substring → matching IDs).
//     SenderGroups is at the message level too — multiple `from`
//     recipient rows on a single message can satisfy different tokens.
//     Matching is solely against `from` recipient rows in
//     message_recipients; messages.sender_id is intentionally NOT
//     consulted, mirroring the canonical FTS filter in
//     internal/store/api.go so the vector and SQLite paths agree.
//   - SubjectSubstrings each add one `m.subject LIKE ? ESCAPE '\'`
//     condition, ANDed together (all substrings must match).
//   - MessageTypes restricts m.message_type by exact value.
//   - After/Before are half-open against m.sent_at:
//     `>= After` and `< Before`.
//   - LargerThan/SmallerThan compare against m.size_estimate.
type Filter struct {
	SourceIDs         []int64   // from [server/sources].identifier; empty = no source filter
	SenderGroups      [][]int64 // one inner slice per `from:` token; AND across, OR within
	ToGroups          [][]int64 // one inner slice per `to:` token; AND across, OR within
	CcGroups          [][]int64 // one inner slice per `cc:` token; AND across, OR within
	BccGroups         [][]int64 // one inner slice per `bcc:` token; AND across, OR within
	LabelGroups       [][]int64 // one inner slice per `label:` token; AND across, OR within
	HasAttachment     *bool
	After, Before     *time.Time
	LargerThan        *int64   // `larger:` — strictly greater than
	SmallerThan       *int64   // `smaller:` — strictly less than
	SubjectSubstrings []string // one per `subject:` term (ANDed)
	MessageTypes      []string // exact m.message_type values; empty = unrestricted
}

// IsEmpty reports whether the filter has no restrictions. A zero-value
// Filter is empty and backends should skip filter resolution entirely.
func (f Filter) IsEmpty() bool {
	return len(f.SourceIDs) == 0 &&
		len(f.SenderGroups) == 0 &&
		len(f.ToGroups) == 0 &&
		len(f.CcGroups) == 0 &&
		len(f.BccGroups) == 0 &&
		len(f.LabelGroups) == 0 &&
		f.HasAttachment == nil &&
		f.After == nil &&
		f.Before == nil &&
		f.LargerThan == nil &&
		f.SmallerThan == nil &&
		len(f.SubjectSubstrings) == 0 &&
		len(f.MessageTypes) == 0
}

// Hit is one search result.
type Hit struct {
	MessageID int64
	Score     float64 // backend-native score
	Rank      int     // 1-based rank within this signal
}

// ChunkHit scores one embedded chunk against a query vector.
// ChunkCharStart/ChunkCharEnd are rune offsets into the preprocessed
// embed text (subject prefix + body), matching embeddings.chunk_char_*.
type ChunkHit struct {
	ChunkIndex     int
	ChunkCharStart int
	ChunkCharEnd   int
	Score          float64 // backend-native; higher is better (1 - distance)
}

// ChunkScoringBackend scores every embedded chunk of a message against a
// query vector. sqlitevec and pgvector backends implement this.
type ChunkScoringBackend interface {
	Backend
	ScoreMessageChunks(ctx context.Context, gen GenerationID, messageID int64, queryVec []float32) ([]ChunkHit, error)
}

// Stats reports the size of one generation (or 0 for totals).
type Stats struct {
	EmbeddingCount int64
	// PendingCount, under the scan-and-fill design, is the number of live
	// messages still needing embedding for this generation (embed_gen <>
	// gen), computed from the main DB rather than a queue table. It is 0
	// for the aggregate (gen == 0) path. The name is retained for API
	// stability; semantically it is now a "missing" count.
	PendingCount int64
	StorageBytes int64
}

// Backend is the minimum contract a vector store must implement.
type Backend interface {
	// CreateGeneration starts (or resumes) a building generation.
	// fingerprint is stored verbatim on the row; pass
	// Config.GenerationFingerprint() so a later config change (model,
	// dimension, or any preprocess toggle) trips
	// ResolveActiveForFingerprint and forces a --full-rebuild instead
	// of silently mixing inconsistently-prepared vectors.
	CreateGeneration(ctx context.Context, model string, dimension int, fingerprint string) (GenerationID, error)

	// ActivateGeneration atomically retires the current active generation
	// (if any, deleting its embeddings on backends that share an index
	// graph) and promotes gen to active. The promotion enforces that gen is
	// in state='building' and — unless force is true — that gen has full
	// coverage (no live message still needs embedding for it, i.e.
	// missing==0). On PG the coverage gate is folded into the same
	// transaction as the state flip; on SQLite (cross-DB) it is a Go
	// pre-check before the flip, with the full-scan backstop covering the
	// TOCTOU window. force bypasses the coverage gate (operator `--force`).
	// On a gate failure the backend returns a precise error distinguishing
	// missing-coverage vs not-building.
	ActivateGeneration(ctx context.Context, gen GenerationID, force bool) error

	// RetireGeneration marks gen as retired (a state flip on its
	// index_generations row), and on backends that share an index graph
	// (pgvector) also deletes the generation's embeddings so the shared HNSW
	// graph stays generation-clean. (There is no pending queue to reap under
	// scan-and-fill.) Unless force is true, the state-flip UPDATE refuses to
	// retire a generation in state='active', returning ErrRefuseRetireActive
	// WITHOUT deleting anything; the guard is enforced atomically inside the
	// retire transaction so a concurrent activation between a caller's
	// pre-flight read and the flip cannot retire (and on pgvector delete the
	// embeddings of) the now-serving generation. force bypasses the guard
	// (operator `--force-active`) and retires unconditionally.
	RetireGeneration(ctx context.Context, gen GenerationID, force bool) error

	// ActiveGeneration returns the current active generation, or
	// ErrNoActiveGeneration if none exists.
	ActiveGeneration(ctx context.Context) (Generation, error)
	BuildingGeneration(ctx context.Context) (*Generation, error)

	Upsert(ctx context.Context, gen GenerationID, chunks []Chunk) error
	Search(ctx context.Context, gen GenerationID, queryVec []float32, k int, filter Filter) ([]Hit, error)
	Delete(ctx context.Context, gen GenerationID, messageIDs []int64) error
	Stats(ctx context.Context, gen GenerationID) (Stats, error)

	// EmbeddedMessageCount reports how many distinct in-scope LIVE, stamped
	// (embed_gen == gen) messages actually have at least one embedding row
	// for gen. This is the "embedded" leg of the coverage readout (live /
	// embedded / blank / missing). It lives on the backend because the
	// embeddings table is in vectors.db on SQLite (and the main DB on PG);
	// only the backend holds that handle. The live+stamped+scope intersection
	// is REQUIRED for the coverage invariant to hold: SQLite intersects the
	// vectors.db embedding ids against an in-scope live+stamped query on the
	// main DB (cross-DB json_each, mirroring dropDeletedFromSource), while
	// PostgreSQL uses a single JOIN to messages. Distinct from
	// Stats.EmbeddingCount only in intent: this is the dedicated coverage
	// helper and never folds the aggregate (gen == 0) path.
	EmbeddedMessageCount(ctx context.Context, gen GenerationID) (int64, error)

	// LoadVector returns the embedding for a specific message in the
	// active generation. Returns ErrNoActiveGeneration if none exists, or
	// a descriptive error if the message isn't embedded in the active
	// generation.
	LoadVector(ctx context.Context, messageID int64) ([]float32, error)

	// ResetWatermarkBelow lowers the scan-and-fill forward-scan watermark
	// for EVERY generation to at most minID-1 (clamped at 0), so the next
	// incremental RunOnce re-scans from below minID and re-finds rows whose
	// embed_gen was just reset to NULL (repair-encoding). Without it, a
	// repaired message whose id sits BELOW the current watermark would never
	// be re-found by an incremental scan (the scan applies `id > watermark`)
	// and would only be recovered by a full-scan backstop. Lowering is a MIN
	// against the stored watermark, so it never pushes the cursor FORWARD past
	// unswept work; a row already at/below the watermark is left untouched.
	// minID < 1 is a no-op. The watermark lives in vectors.db on SQLite and
	// the main DB on PostgreSQL, so each backend implements it against its
	// own handle/dialect. Idempotent.
	ResetWatermarkBelow(ctx context.Context, minID int64) error

	Close() error
}

// FusingBackend is an optional capability implemented by backends that
// can fuse FTS5 + ANN in a single SQL query. The hybrid engine checks
// for this via type assertion.
//
// FusedSearch returns the RRF-ordered hits, a saturation flag, and
// any error. saturated is true when either the BM25 or the ANN
// per-signal pool produced MORE THAN KPerSignal candidates — each pool
// is over-fetched by one probe row (cap KPerSignal+1) and that probe
// slot filled, so the final result set may have truncated
// potentially-relevant hits. (The over-fetch/probe is the implementation's
// chosen way to detect the cap; both concrete backends use it.) Callers
// surface this to clients as pool_saturated so the user can raise
// KPerSignal or narrow the query.
type FusingBackend interface {
	Backend
	FusedSearch(ctx context.Context, req FusedRequest) (hits []FusedHit, saturated bool, err error)
}

// FusedRequest is the parameter bundle for a single-query fused hybrid search.
type FusedRequest struct {
	// FTSTerms are dialect-neutral, already-tokenized and
	// punctuation-filtered search terms. An empty/nil slice skips the
	// BM25 leg (vector-only). Each FusingBackend renders the terms via
	// its own query dialect's BuildFTSTerm (SQLite FTS5 MATCH;
	// PostgreSQL to_tsquery with :* prefix lexemes), so both backends
	// prefix-match the SAME term set rather than diverging on a
	// pre-built dialect-specific expression.
	FTSTerms     []string
	QueryVec     []float32 // query embedding; nil skips ANN
	Generation   GenerationID
	KPerSignal   int
	Limit        int
	RRFK         int
	SubjectBoost float64
	SubjectTerms []string // lowercased query terms used for subject-boost check
	Filter       Filter
}

// FusedHit is one result from a fused query. BM25Score/VectorScore are
// NaN when the message did not appear in that signal.
type FusedHit struct {
	MessageID      int64
	RRFScore       float64
	BM25Score      float64 // math.NaN() if missing
	VectorScore    float64 // math.NaN() if missing
	SubjectBoosted bool
}
