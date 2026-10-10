package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/vector"
)

type interleavingCoverageBackend struct {
	vector.Backend

	snapshot vector.CoverageSnapshotBackend
	commit   func(context.Context) error
}

func (b *interleavingCoverageBackend) commitBeforeCount(ctx context.Context) error {
	if b.commit == nil {
		return nil
	}
	commit := b.commit
	b.commit = nil
	return commit(ctx)
}

func (b *interleavingCoverageBackend) EmbeddedMessageCount(ctx context.Context, gen vector.GenerationID) (int64, error) {
	if err := b.commitBeforeCount(ctx); err != nil {
		return 0, err
	}
	return b.Backend.EmbeddedMessageCount(ctx, gen)
}

func (b *interleavingCoverageBackend) EmbeddedMessageCountForSnapshot(
	ctx context.Context, gen vector.GenerationID, stampedMessageIDs []int64,
) (int64, error) {
	if err := b.commitBeforeCount(ctx); err != nil {
		return 0, err
	}
	return b.snapshot.EmbeddedMessageCountForSnapshot(ctx, gen, stampedMessageIDs)
}
