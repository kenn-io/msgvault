package cmd

import (
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// vectorFeatures carries the optional vector-search components that the
// serve, mcp, sync, and sync-full commands wire into their servers and
// sync pipelines. It is populated only when cfg.Vector.Enabled is true
// AND the binary is built with a vector backend tag (sqlite_vec or
// pgvector); otherwise setupVectorFeatures returns (nil, nil) or a clear
// error.
//
// When non-nil, all fields are populated (invariant enforced by
// setupVectorFeatures). Callers only need to nil-check vf itself.
type vectorFeatures struct {
	Backend      vector.Backend
	HybridEngine *hybrid.Engine
	Worker       *embed.Worker
	Cfg          vector.Config
	// Close releases the backend's resources: on SQLite it closes the
	// vectors.db handle (so WAL checkpoints complete); on PostgreSQL it is
	// a no-op because the pgvector backend shares the main store's handle,
	// which is owned and closed elsewhere. Every caller that receives a
	// non-nil vectorFeatures must invoke Close during shutdown.
	Close func() error
}
