//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// lowerEmbedWatermarkForRepair lowers the scan-and-fill embed watermark below
// the minimum repaired message id so the next incremental embed run re-finds
// the repaired messages, even when their ids sit BELOW the current watermark.
//
// repair-encoding already reset embed_gen=NULL on these ids (s.ResetEmbedGen),
// but ScanForEmbedding only returns rows with `id > watermark`, so a repaired
// row below the watermark would otherwise wait for a full-scan backstop (which
// the CLI defaults off and serve can have disabled). Lowering the watermark
// closes that gap.
//
// Skips silently (returns nil) when vector search is not configured
// (cfg.Vector.Enabled == false): a user without embeddings has no watermark to
// fix. Opens the vector backend the same dialect-selected way the embed/serve
// commands do, lowers the watermark for every generation, and closes it. The
// reset is idempotent and never raises a generation's cursor.
//
// This file is compiled only with a vector backend build tag; the no-tag build
// uses the stub in repair_encoding_vector_stub.go.
func lowerEmbedWatermarkForRepair(ctx context.Context, s *store.Store, minRepairedID int64) error {
	if !cfg.Vector.Enabled {
		// Vector search disabled: no watermark exists to lower. No-op.
		return nil
	}

	var (
		backend vector.Backend
		closeFn func() error
	)
	if s.IsPostgreSQL() {
		pgb, err := pgvector.Open(ctx, pgvector.Options{
			DB:            s.DB(),
			Dimension:     cfg.Vector.Embeddings.Dimension,
			SkipExtension: cfg.Vector.SkipExtensionCreate,
		})
		if err != nil {
			return fmt.Errorf("open pgvector backend: %w", err)
		}
		backend = pgb
		closeFn = pgb.Close
	} else {
		if err := sqlitevec.RegisterExtension(); err != nil {
			return fmt.Errorf("register sqlite-vec: %w", err)
		}
		vecPath := cfg.Vector.DBPath
		if vecPath == "" {
			vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
		}
		sb, err := sqlitevec.Open(ctx, sqlitevec.Options{
			Path:      vecPath,
			MainPath:  cfg.DatabaseDSN(),
			Dimension: cfg.Vector.Embeddings.Dimension,
			MainDB:    s.DB(),
		})
		if err != nil {
			return fmt.Errorf("open vectors.db: %w", err)
		}
		backend = sb
		closeFn = sb.Close
	}
	defer func() { _ = closeFn() }()

	if err := backend.ResetWatermarkBelow(ctx, minRepairedID); err != nil {
		return fmt.Errorf("lower embed watermark: %w", err)
	}
	return nil
}
