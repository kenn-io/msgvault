package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"go.kenn.io/msgvault/internal/vector/visual"
)

type visualFeatures struct {
	Archive    *store.Store
	Backend    visual.Backend
	Provider   visual.Provider
	Reconciler *visual.Reconciler
	Worker     *visual.Worker
	Generation store.VisualGeneration
	// PolicyFingerprint is the docbank Voyage policy identity of the manifest
	// in force; consent is recorded against and verified with it.
	PolicyFingerprint string
	// ScopeCheck re-resolves the configured account scope against the live
	// archive before any provider-capable pass. SQLite can reuse a deleted
	// account's numeric ID, so cached source IDs must never outlive the
	// account list they were resolved from.
	ScopeCheck func(context.Context) error
}

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
	// PersonSearchEngine shares Backend and Cfg with HybridEngine, but searches
	// only the person-owned corpus through a separately gated provider client.
	PersonSearchEngine *personsearch.Engine
	Runner             scheduler.EmbedRunner
	Convergence        scheduler.ConvergenceChecker
	Cfg                vector.Config
	Visual             *visualFeatures
	// Close releases the backend's resources: on SQLite it closes the
	// vectors.db handle (so WAL checkpoints complete); on PostgreSQL it is
	// a no-op because the pgvector backend shares the main store's handle,
	// which is owned and closed elsewhere. Every caller that receives a
	// non-nil vectorFeatures must invoke Close during shutdown.
	Close func() error
}
