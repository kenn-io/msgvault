//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func TestMessageCosinesScoresAnyVectorLength(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 768, MainDB: openMainDBWithOneMessage(t)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	gen, err := b.CreateGeneration(ctx, "model", 768, "")
	require.NoError(t, err)
	vec := func(x, y float32) []float32 {
		v := make([]float32, 768)
		v[0], v[1] = x, y
		return v
	}
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: vec(3, 0)},
		{MessageID: 2, ChunkIndex: 0, Vector: vec(1, 1)},
		{MessageID: 3, ChunkIndex: 0, Vector: vec(0, 0)},
		{MessageID: 4, ChunkIndex: 0, Vector: vec(0, 2)},
		{MessageID: 4, ChunkIndex: 1, Vector: vec(2, 2)},
	}))

	got, err := b.MessageCosines(ctx, gen, vec(2, 0), []int64{1, 2, 3, 4, 5})
	require.NoError(t, err)
	require.Len(t, got, 4, "a message with no chunk is left out")
	assert.InDelta(t, 1, got[1], 1e-9, "length does not matter")
	assert.InDelta(t, 1/math.Sqrt2, got[2], 1e-6)
	assert.Zero(t, got[3], "a zero vector scores 0")
	assert.InDelta(t, 1/math.Sqrt2, got[4], 1e-6, "the closest chunk scores the message")

	_, err = b.MessageCosines(ctx, gen, []float32{1, 0}, []int64{1})
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)
}
