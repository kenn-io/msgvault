package vector

import "errors"

// Sentinel errors used across the vector package. Callers should use
// errors.Is to check for these.
var (
	// ErrNotEnabled is returned when vector search is requested but
	// [vector] is not configured.
	ErrNotEnabled = errors.New("vector search not enabled")

	// ErrIndexStale is returned when the configured embedding settings
	// differ from the active generation's fingerprint. Settings include the
	// model, preprocessing policy, and embedding scope.
	ErrIndexStale = errors.New("index stale: configured embedding settings do not match active generation")

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

	// ErrScopeUnresolvable marks a DETERMINISTIC failure to re-resolve the
	// durable embedding scope: a configured account was removed, became
	// ambiguous, or otherwise no longer names a source set. Unlike a
	// transient resolution failure (a busy database), this cannot heal on
	// retry — the daemon's drift detection latches vector search stale so
	// queries stop serving an index whose scope no longer matches the
	// configuration.
	ErrScopeUnresolvable = errors.New("embedding scope unresolvable")

	// ErrRefuseActivateEmptyScope is returned by ActivateGeneration when
	// force is false and the backend's source-scoped build scope matches no
	// live messages. Activating would swap in an empty index and auto-retire
	// the serving generation (deleting its embeddings on pgvector), so every
	// non-forced activation path — the CLI drain, the daemon scheduler, and
	// `embeddings activate` — is refused at the backend gate. The usual
	// cause is a scoped account that exists but has never been synced.
	ErrRefuseActivateEmptyScope = errors.New("refusing to activate: the source-scoped build scope matches no live messages")

	// ErrRefuseRetireActive is returned by RetireGeneration when force is
	// false and the target generation is in state='active'. Retiring the
	// serving generation is destructive on backends that delete a retired
	// generation's embeddings (pgvector), so the backend refuses without an
	// explicit force (the CLI surfaces this as `--force-active`). The state
	// guard is enforced atomically inside the retire transaction, so a
	// concurrent activation between a caller's pre-flight read and the flip
	// cannot delete the now-serving generation's embeddings.
	ErrRefuseRetireActive = errors.New("refusing to retire the active (serving) generation without force")

	// ErrEmbeddingTimeout is returned by the hybrid engine when the
	// embedding endpoint did not respond before the request context
	// was cancelled (typically because the HTTP server's per-request
	// timeout elapsed first). Callers should map this to a 503-style
	// "transient backend slow" response so clients can retry instead
	// of treating it as a permanent failure.
	ErrEmbeddingTimeout = errors.New("embedding request timed out")

	// ErrIndexScopeMismatch is returned when a scoped embedding index
	// is used without an equivalent structured filter. For example, an
	// index built only for message_type=sms must not answer an unscoped
	// vector query over email + SMS.
	ErrIndexScopeMismatch = errors.New("index scope mismatch")

	// ErrCoverageBatchTooLarge rejects an analytical coverage intersection
	// that would exceed the fixed backend parameter bound.
	ErrCoverageBatchTooLarge = errors.New("filtered coverage batch too large")

	// ErrGenerationNotConverged is returned when contextual source or cursor
	// state changed after a caller's convergence read but before activation.
	ErrGenerationNotConverged = errors.New("contextual generation no longer converged")

	// ErrDocumentFenceChanged is returned when a fence-only publication finds
	// that another publication changed the scope after source assembly.
	ErrDocumentFenceChanged = errors.New("document scope changed before sequence fence")
)
