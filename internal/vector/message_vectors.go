package vector

import (
	"context"
	"fmt"
	"slices"
)

// MessageVectorReader reads raw stored chunk vectors for selection. It is
// read-only and never embeds. Requests above MaxFilterMessageIDs input IDs
// fail with ErrFilterTooLarge; duplicate IDs are read once. Each message's
// chunks are its first maxChunks in chunk_index order, without normalization.
// Missing messages are absent from the result. Empty IDs return an empty map;
// nonempty requests require positive maxChunks and gen.Dimension.
// gen must come from the backend's own generation lookup.
type MessageVectorReader interface {
	MessageVectors(ctx context.Context, gen Generation, messageIDs []int64, maxChunks int) (map[int64][][]float32, error)
}

// MessageVectorIDs validates a MessageVectorReader request and returns its
// distinct message IDs in ascending order.
func MessageVectorIDs(gen Generation, messageIDs []int64, maxChunks int) ([]int64, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	if len(messageIDs) > MaxFilterMessageIDs {
		return nil, fmt.Errorf("read message vectors: %w: got %d", ErrFilterTooLarge, len(messageIDs))
	}
	if maxChunks < 1 || gen.Dimension < 1 {
		return nil, fmt.Errorf("read message vectors: maxChunks and dimension must be positive: got %d and %d", maxChunks, gen.Dimension)
	}
	ids := slices.Clone(messageIDs)
	slices.Sort(ids)
	return slices.Compact(ids), nil
}
