//go:build !sqlite_vec && !pgvector

package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

// errVectorBuildUnsupported reports that vector search is enabled in
// config but this binary was built without the vector backend needed for
// mainPath's dialect. Shared by setupVectorFeatures and
// precheckVectorFeatures so both surface the identical rebuild guidance.
func errVectorBuildUnsupported(mainPath string) error {
	// Point the user at the build tags they need: sqlite_vec for the
	// SQLite backend, plus pgvector for the PostgreSQL backend.
	if store.IsPostgresURL(mainPath) {
		return errors.New("vector search is enabled in config but this binary was built without vector support; " +
			"to use vector search on PostgreSQL, rebuild with `go build -tags \"fts5 sqlite_vec pgvector\"` " +
			"or set [vector] enabled = false")
	}
	return errors.New("vector search is enabled in config but this binary was built without -tags sqlite_vec; " +
		"rebuild with `make build` (or `go build -tags \"fts5 sqlite_vec\"`) " +
		"or set [vector] enabled = false")
}

// setupVectorFeatures is the no-sqlite-vec fallback. It returns
// (nil, nil) when vector search is disabled, and a descriptive error
// when the user enabled vector search in config but built the binary
// without -tags sqlite_vec.
func setupVectorFeatures(_ context.Context, _ *store.Store, mainPath string, _ bool) (*vectorFeatures, error) {
	if !cfg.Vector.Enabled {
		return nil, nil //nolint:nilnil // vector disabled: callers nil-check vf; (nil, nil) means "no features, no error"
	}
	return nil, errVectorBuildUnsupported(mainPath)
}

// precheckVectorFeatures is the no-sqlite-vec fallback's cheap precheck.
// It mirrors setupVectorFeatures's enabled/disabled gate without the
// backend construction, since the stub build never has a backend to
// build.
func precheckVectorFeatures(mainPath string) error {
	if !cfg.Vector.Enabled {
		return nil
	}
	return errVectorBuildUnsupported(mainPath)
}
