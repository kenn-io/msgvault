package novelty

import (
	"math"
	"math/big"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageVectorMeanOfFirstChunks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	got, ok := MessageVector([][]float32{{2, 0, 0, 0, 0}, {0, 1, 0, 0, 0}, {0, 0, 1, 0, 0}, {0, 0, 0, 1, 0}, {0, 0, 0, 0, 1}}, 5, 4)
	require.True(ok)
	for i, want := range []float32{.5, .5, .5, .5, 0} {
		assert.InDelta(want, got[i], 1e-6)
	}
	got, ok = MessageVector([][]float32{{3, 4}}, 2, 4)
	require.True(ok)
	assert.InDeltaSlice([]float32{.6, .8}, got, 1e-6)
}

func TestMessageVectorSkipsZeroAndRejectsNonFinite(t *testing.T) {
	assert := assert.New(t)
	got, ok := MessageVector([][]float32{{0, 0}, {0, 1}}, 2, 4)
	require.True(t, ok)
	assert.Equal([]float32{0, 1}, got)
	for _, chunks := range [][][]float32{{{0, 0}}, {{float32(math.NaN()), 1}}, {{float32(math.Inf(1)), 1}}, {{float32(math.Inf(-1)), 1}}, {{1}}, {{1, 0}, {-1, 0}}, nil} {
		_, ok = MessageVector(chunks, 2, 4)
		assert.False(ok)
	}
	_, ok = MessageVector([][]float32{{1}}, 0, 4)
	assert.False(ok)
	_, ok = MessageVector([][]float32{{1}}, 1, 0)
	assert.False(ok)
}

func referenceDot(a, b []float64) float64 {
	sum := new(big.Float).SetPrec(53).SetMode(big.ToNearestEven)
	for i, x := range a {
		product := new(big.Float).SetPrec(53).SetMode(big.ToNearestEven)
		product.Mul(new(big.Float).SetFloat64(x), new(big.Float).SetFloat64(b[i]))
		sum.Add(sum, product)
	}
	v, _ := sum.Float64()
	return v
}

func TestNorm64MatchesUnfusedReference(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // Deterministic synthetic fixtures, not cryptographic randomness.
	for range 2000 {
		a, b := make([]float64, 64), make([]float64, 64)
		a32, b32 := make([]float32, 64), make([]float32, 64)
		for i := range a {
			a[i] = r.NormFloat64()
			a32[i] = float32(a[i])
			b32[i] = float32(r.NormFloat64())
			b[i] = float64(b32[i])
		}
		// Compare bits so the unfused rounding contract remains exact.
		assert.Equal(t, math.Float64bits(math.Sqrt(referenceDot(a, a))), math.Float64bits(norm64(a)))
		for i := range a {
			a[i] = float64(a32[i])
		}
		assert.Equal(t, math.Float64bits(math.Sqrt(referenceDot(a, a))), math.Float64bits(norm32(a32)))
		assert.Equal(t, math.Float64bits(referenceDot(a, b)), math.Float64bits(dot(a32, b32)))
	}
}

func TestMicroQuantizes(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(int64(980000), micro(.9800004))
	assert.Equal(int64(-1000000), micro(-1))
	assert.Equal(int64(1000000), clampMicro(1500000))
	assert.Equal(int64(0), clampMicro(-1))
}
