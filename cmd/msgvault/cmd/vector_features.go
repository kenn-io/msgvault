package cmd

import (
	"database/sql"

	"go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// vectorFeatures carries the optional vector-search components that the
// serve, mcp, sync, and sync-full commands wire into their servers and
// sync pipelines. It is populated only when cfg.Vector.Enabled is true
// AND the binary is built with -tags sqlite_vec; otherwise
// setupVectorFeatures returns (nil, nil) or a clear error.
//
// When non-nil, all fields are populated (invariant enforced by
// setupVectorFeatures). Callers only need to nil-check vf itself.
type vectorFeatures struct {
	Backend      vector.Backend
	HybridEngine *hybrid.Engine
	Enqueuer     sync.EmbedEnqueuer
	Worker       *embed.Worker
	Cfg          vector.Config
	// VectorsDB is the underlying vectors.db handle. The daemon's
	// EmbedJob uses it to count pending_embeddings for the
	// activation gate; other consumers should prefer the higher-
	// level Backend abstraction.
	VectorsDB *sql.DB
	// Close releases the underlying vectors.db handle. Every caller
	// that receives a non-nil vectorFeatures must invoke Close during
	// shutdown so WAL checkpoints complete.
	Close func() error
}
