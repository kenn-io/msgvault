//go:build sqlite_vec

package cmd

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func readEmbeddingAccelerator(
	ctx context.Context, backend vector.Backend, generationID vector.GenerationID,
) (embeddingAcceleratorRow, error) {
	concrete, ok := backend.(*sqlitevec.Backend)
	if !ok {
		return embeddingAcceleratorRow{}, nil
	}
	// EffectiveAccelerator applies the same eligibility checks search uses:
	// a stored-ready accelerator search would refuse (revision or count
	// drift, Vec1 version mismatch, missing physical table) displays as
	// stale instead of the raw stored ready state.
	status, err := concrete.EffectiveAccelerator(ctx, generationID)
	if err != nil {
		return embeddingAcceleratorRow{}, fmt.Errorf("read generation %d accelerator status: %w", generationID, err)
	}
	if status == nil {
		return embeddingAcceleratorRow{State: "exact"}, nil
	}
	started := status.StartedAt
	return embeddingAcceleratorRow{
		State: string(status.State), IndexedCount: status.IndexedCount,
		StartedAt: &started, CompletedAt: status.CompletedAt, LastError: status.LastError,
	}, nil
}
