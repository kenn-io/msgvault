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
//     recipient rows on a single message can satisfy different tokens,
//     and the message's sender_id is also considered for each group
//     (legacy rows where sender_id is NULL fall back to a `from`
//     recipient row).
//   - SubjectSubstrings each add one `m.subject LIKE ? ESCAPE '\'`
//     condition, ANDed together (all substrings must match).
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
		len(f.SubjectSubstrings) == 0
}

// Hit is one search result.
type Hit struct {
	MessageID int64
	Score     float64 // backend-native score
	Rank      int     // 1-based rank within this signal
}

// Stats reports the size of one generation (or 0 for totals).
type Stats struct {
	EmbeddingCount int64
	PendingCount   int64
	StorageBytes   int64
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
	ActivateGeneration(ctx context.Context, gen GenerationID) error
	RetireGeneration(ctx context.Context, gen GenerationID) error

	// ActiveGeneration returns the current active generation, or
	// ErrNoActiveGeneration if none exists.
	ActiveGeneration(ctx context.Context) (Generation, error)
	BuildingGeneration(ctx context.Context) (*Generation, error)

	Upsert(ctx context.Context, gen GenerationID, chunks []Chunk) error
	Search(ctx context.Context, gen GenerationID, queryVec []float32, k int, filter Filter) ([]Hit, error)
	Delete(ctx context.Context, gen GenerationID, messageIDs []int64) error
	Stats(ctx context.Context, gen GenerationID) (Stats, error)

	// EnsureSeeded guarantees that the building generation gen has had
	// its initial pending_embeddings seed pass committed. If a prior
	// CreateGeneration crashed between inserting the building row and
	// committing the seed, the queue would be empty and a naive resume
	// could "drain" zero rows and activate an unseeded index.
	// EnsureSeeded re-runs the seed (idempotent — INSERT OR IGNORE) and
	// stamps seeded_at when it commits. Call this on the resume path
	// before draining the queue. Returns ErrUnknownGeneration if gen no
	// longer exists in the index, and an error if gen is not in the
	// `building` state.
	EnsureSeeded(ctx context.Context, gen GenerationID) error

	// LoadVector returns the embedding for a specific message in the
	// active generation. Returns ErrNoActiveGeneration if none exists, or
	// a descriptive error if the message isn't embedded in the active
	// generation.
	LoadVector(ctx context.Context, messageID int64) ([]float32, error)

	Close() error
}

// FusingBackend is an optional capability implemented by backends that
// can fuse FTS5 + ANN in a single SQL query. The hybrid engine checks
// for this via type assertion.
//
// FusedSearch returns the RRF-ordered hits, a saturation flag, and
// any error. saturated is true when either the BM25 or the ANN
// per-signal pool produced KPerSignal candidates — the pool hit its
// cap, and the final result set has truncated potentially-relevant
// hits. Callers surface this to clients as pool_saturated so the
// user can raise KPerSignal or narrow the query.
type FusingBackend interface {
	Backend
	FusedSearch(ctx context.Context, req FusedRequest) (hits []FusedHit, saturated bool, err error)
}

// FusedRequest is the parameter bundle for a single-query fused hybrid search.
type FusedRequest struct {
	FTSQuery     string    // pre-tokenized FTS5 MATCH expression; empty skips BM25
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
