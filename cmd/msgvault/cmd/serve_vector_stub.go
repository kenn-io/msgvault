//go:build !sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

// setupVectorFeatures is the no-sqlite-vec fallback. It returns
// (nil, nil) when vector search is disabled, and a descriptive error
// when the user enabled vector search in config but built the binary
// without -tags sqlite_vec.
func setupVectorFeatures(_ context.Context, _ *sql.DB, mainPath string) (*vectorFeatures, error) {
	if !cfg.Vector.Enabled {
		return nil, nil //nolint:nilnil // vector disabled: callers nil-check vf; (nil, nil) means "no features, no error"
	}
	// Mirror the PG refusal in the sqlite_vec build so users get the
	// same actionable message regardless of how the binary was built.
	if store.IsPostgresURL(mainPath) {
		return nil, errors.New("vector features are SQLite-only; set [vector] enabled = false to use msgvault with PostgreSQL (vector support is planned for PR4)")
	}
	return nil, errors.New("vector search is enabled in config but this binary was built without -tags sqlite_vec; " +
		"rebuild with `make build` (or `go build -tags \"fts5 sqlite_vec\"`) " +
		"or set [vector] enabled = false")
}
