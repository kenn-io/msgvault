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

// openVectorBackendForRepair opens the dialect-selected vector backend the same
// way the embed/serve commands do and returns it together with a close func.
//
// Opening a writable backend runs the one-time upgrade backfill
// (BackfillEmbedGenForUpgrade) as a side effect when the upgrade ledger is
// still unmarked. repair-encoding MUST open the backend BEFORE it clears
// embed_gen (s.ResetEmbedGen): if the reset ran first, a first-run backfill
// would re-stamp the just-NULLed (previously-embedded) messages back to
// embed_gen=active, silently undoing the re-embed request. Opening first lets
// the backfill land and mark its ledger, so the subsequent reset sticks.
//
// Returns (nil, nil, nil) and is a silent no-op when vector search is not
// configured (cfg.Vector.Enabled == false): a user without embeddings has no
// watermark to fix and no backfill to run.
//
// This file is compiled only with a vector backend build tag; the no-tag build
// uses the stub in repair_encoding_vector_stub.go.
func openVectorBackendForRepair(ctx context.Context, s *store.Store) (vector.Backend, func() error, error) {
	if !cfg.Vector.Enabled {
		// Vector search disabled: nothing to open. No-op.
		return nil, nil, nil
	}

	if s.IsPostgreSQL() {
		pgb, err := pgvector.Open(ctx, pgvector.Options{
			DB:            s.DB(),
			Dimension:     cfg.Vector.Embeddings.Dimension,
			SkipExtension: cfg.Vector.SkipExtensionCreate,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("open pgvector backend: %w", err)
		}
		return pgb, pgb.Close, nil
	}

	if err := sqlitevec.RegisterExtension(); err != nil {
		return nil, nil, fmt.Errorf("register sqlite-vec: %w", err)
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
		return nil, nil, fmt.Errorf("open vectors.db: %w", err)
	}
	return sb, sb.Close, nil
}

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
// It operates on an already-open backend handle (see
// openVectorBackendForRepair) so the one-time upgrade backfill has already run
// and marked its ledger; the watermark reset itself is idempotent and never
// raises a generation's cursor. backend may be nil when vector search is
// disabled, in which case this is a no-op.
//
// This file is compiled only with a vector backend build tag; the no-tag build
// uses the stub in repair_encoding_vector_stub.go.
func lowerEmbedWatermarkForRepair(ctx context.Context, backend vector.Backend, minRepairedID int64) error {
	if backend == nil {
		// Vector search disabled: no watermark exists to lower. No-op.
		return nil
	}
	if err := backend.ResetWatermarkBelow(ctx, minRepairedID); err != nil {
		return fmt.Errorf("lower embed watermark: %w", err)
	}
	return nil
}
