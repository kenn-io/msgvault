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
	main := openMainDBWithOneMessage(t)
	_, err := main.Exec(`INSERT INTO messages (id) VALUES (2), (3), (4)`)
	require.NoError(t, err)
	b, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 768, MainDB: main})
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

	got, err := b.MessageCosines(ctx, vector.Generation{ID: gen, Dimension: 768}, vec(1, 0), []int64{1, 2, 3, 4, 5})
	require.NoError(t, err)
	require.Len(t, got.Scores, 4, "a message with no chunk is left out")
	assert.InDelta(t, 1, got.Scores[1], 1e-9, "length does not matter")
	assert.InDelta(t, 1/math.Sqrt2, got.Scores[2], 1e-6)
	assert.Zero(t, got.Scores[3], "a zero vector scores 0")
	assert.InDelta(t, 1/math.Sqrt2, got.Scores[4], 1e-6, "the closest chunk scores the message")
}

// UnitLength says whether L2 order follows cosine order for the vectors read.
func TestMessageCosinesReportsUnitLength(t *testing.T) {
	ctx := context.Background()
	main := openMainDBWithOneMessage(t)
	_, err := main.Exec(`INSERT INTO messages (id) VALUES (2)`)
	require.NoError(t, err)
	b, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 768, MainDB: main})
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
		{MessageID: 1, ChunkIndex: 0, Vector: vec(.6, .8)},
		{MessageID: 2, ChunkIndex: 0, Vector: vec(3, 0)},
	}))

	for name, tc := range map[string]struct {
		query []float32
		ids   []int64
		want  bool
	}{
		"unit query and vector": {vec(1, 0), []int64{1}, true},
		"scaled query":          {vec(2, 0), []int64{1}, false},
		"scaled stored vector":  {vec(1, 0), []int64{1, 2}, false},
		"scaled query, no ids":  {vec(2, 0), nil, false},
	} {
		got, err := b.MessageCosines(ctx, vector.Generation{ID: gen, Dimension: 768}, tc.query, tc.ids)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got.UnitLength, name)
	}
}
