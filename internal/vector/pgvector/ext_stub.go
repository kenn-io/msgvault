//go:build !pgvector

// Package pgvector is a stub when the pgvector build tag is not set.
// The real implementation in backend.go wires up the pgvector backend
// against a PostgreSQL database with the pgvector extension installed.
package pgvector

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/msgvault/internal/vector"
)

// ErrNotBuilt is returned when pgvector features are used in a build
// that did not set the `pgvector` build tag.
var ErrNotBuilt = errors.New(
	"pgvector support not compiled in; rebuild with `go build -tags \"fts5 sqlite_vec pgvector\"`")

// Options mirrors the shape of the real pgvector.Options so non-pgvector
// builds can compile call sites that reference it. None of the fields
// are read because Open() always fails.
type Options struct {
	DB            *sql.DB
	Dimension     int
	SkipMigrate   bool
	ReadOnly      bool
	SkipExtension bool
	BuildScope    vector.BuildScope
}

// Backend is a placeholder type so non-pgvector builds can compile
// against the same package surface. Methods exist only to satisfy the
// vector.Backend interface and always return ErrNotBuilt.
type Backend struct{}

// Open reports that pgvector is unavailable in this build.
func Open(_ context.Context, _ Options) (*Backend, error) { return nil, ErrNotBuilt }

// Available reports whether this build includes the pgvector backend.
func Available() bool { return false }

// Close is a no-op for the stub.
func (b *Backend) Close() error { return nil }

// DB returns nil for the stub.
func (b *Backend) DB() *sql.DB { return nil }

// CreateGeneration always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) CreateGeneration(_ context.Context, _ string, _ int, _ string) (vector.GenerationID, error) {
	return 0, ErrNotBuilt
}

// ActivateGeneration always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) ActivateGeneration(_ context.Context, _ vector.GenerationID, _ bool) error {
	return ErrNotBuilt
}

// RetireGeneration always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) RetireGeneration(_ context.Context, _ vector.GenerationID, _ bool) error {
	return ErrNotBuilt
}

// ActiveGeneration always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) ActiveGeneration(_ context.Context) (vector.Generation, error) {
	return vector.Generation{}, ErrNotBuilt
}

// BuildingGeneration always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) BuildingGeneration(_ context.Context) (*vector.Generation, error) {
	return nil, ErrNotBuilt
}

// Upsert always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) Upsert(_ context.Context, _ vector.GenerationID, _ []vector.Chunk) error {
	return ErrNotBuilt
}

// Search always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) Search(_ context.Context, _ vector.GenerationID, _ []float32, _ int, _ vector.Filter) ([]vector.Hit, error) {
	return nil, ErrNotBuilt
}

// Delete always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) Delete(_ context.Context, _ vector.GenerationID, _ []int64) error {
	return ErrNotBuilt
}

// Stats always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) Stats(_ context.Context, _ vector.GenerationID) (vector.Stats, error) {
	return vector.Stats{}, ErrNotBuilt
}

// LoadVector always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) LoadVector(_ context.Context, _ int64) ([]float32, error) {
	return nil, ErrNotBuilt
}

// ResetWatermarkBelow always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) ResetWatermarkBelow(_ context.Context, _ int64) error {
	return ErrNotBuilt
}

// EmbeddedMessageCount always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) EmbeddedMessageCount(_ context.Context, _ vector.GenerationID) (int64, error) {
	return 0, ErrNotBuilt
}

// ScoreMessageChunks always returns ErrNotBuilt in non-pgvector builds.
func (b *Backend) ScoreMessageChunks(_ context.Context, _ vector.GenerationID, _ int64, _ []float32) ([]vector.ChunkHit, error) {
	return nil, ErrNotBuilt
}

// Compile-time check that the stub matches the vector.Backend
// interface. Keeping the assertion here means changes to the interface
// break stub and real builds in lockstep.
var _ vector.Backend = (*Backend)(nil)
