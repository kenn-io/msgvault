package vector

import "errors"

// Sentinel errors used across the vector package. Callers should use
// errors.Is to check for these.
var (
	// ErrNotEnabled is returned when vector search is requested but
	// [vector] is not configured.
	ErrNotEnabled = errors.New("vector search not enabled")

	// ErrIndexStale is returned when the configured model/dimension
	// differs from the active generation's fingerprint.
	ErrIndexStale = errors.New("index stale: configured model does not match active generation")

	// ErrIndexBuilding is returned when no active generation exists and
	// a first-ever rebuild is in progress.
	ErrIndexBuilding = errors.New("index building: no active generation yet")

	// ErrNoActiveGeneration is returned internally when no generation is
	// in state='active'. Usually surfaced as ErrNotEnabled or ErrIndexBuilding.
	ErrNoActiveGeneration = errors.New("no active generation")

	// ErrDimensionMismatch is returned when a query or chunk vector has
	// a dimension different from the index.
	ErrDimensionMismatch = errors.New("dimension mismatch")

	// ErrPaginationUnsupported is returned for page>1 in vector/hybrid modes.
	ErrPaginationUnsupported = errors.New("pagination not supported for this mode")

	// ErrUnknownGeneration is returned when a caller references a
	// generation ID that does not exist in index_generations.
	ErrUnknownGeneration = errors.New("unknown generation")

	// ErrGenerationRetired is returned by Upsert when the target
	// generation has already been retired. A retired generation's
	// embeddings may have been deleted (pgvector deletes them so the
	// shared HNSW graph stays generation-clean), so writing to it would
	// re-pollute the index and drift message_count. Callers (e.g. a
	// stale embed worker whose claims were reclaimed) should treat this
	// as a benign "drop the batch" signal rather than a hard failure.
	ErrGenerationRetired = errors.New("generation is retired")

	// ErrBuildingInProgress is returned when CreateGeneration is called
	// while another generation is already being built with a different
	// fingerprint, so the caller can surface an actionable message
	// instead of a raw unique-index violation.
	ErrBuildingInProgress = errors.New("a rebuild with a different fingerprint is already in progress")

	// ErrRefuseRetireActive is returned by RetireGeneration when force is
	// false and the target generation is in state='active'. Retiring the
	// serving generation is destructive on backends that delete a retired
	// generation's embeddings (pgvector), so the backend refuses without an
	// explicit force (the CLI surfaces this as `--force-active`). The state
	// guard is enforced atomically inside the retire transaction, so a
	// concurrent activation between a caller's pre-flight read and the flip
	// cannot delete the now-serving generation's embeddings.
	ErrRefuseRetireActive = errors.New("refusing to retire the active (serving) generation without force")

	// ErrGenerationNotBuilding is returned by EnsureSeeded when the
	// target generation is no longer in state='building' — e.g. a
	// concurrent activation flipped it to active, or a retire call
	// moved it to retired, between the caller's BuildingGeneration
	// read and EnsureSeeded. Callers performing a resume can treat
	// this as a retryable race and re-resolve the active/building
	// state instead of aborting.
	ErrGenerationNotBuilding = errors.New("generation is not in state=building")

	// ErrEmbeddingTimeout is returned by the hybrid engine when the
	// embedding endpoint did not respond before the request context
	// was cancelled (typically because the HTTP server's per-request
	// timeout elapsed first). Callers should map this to a 503-style
	// "transient backend slow" response so clients can retry instead
	// of treating it as a permanent failure.
	ErrEmbeddingTimeout = errors.New("embedding request timed out")
)
