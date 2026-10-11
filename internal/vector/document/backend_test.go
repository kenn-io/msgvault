package document

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestDocumentVectorRequestValidation(t *testing.T) {
	one := []float32{1}
	put := func(tokens ...string) []Embedding {
		embeddings := make([]Embedding, 0, len(tokens))
		for _, token := range tokens {
			embeddings = append(embeddings, Embedding{Token: token, Vector: one})
		}
		return embeddings
	}
	withVector := func(values ...float32) []Embedding { return []Embedding{{Token: "t", Vector: values}} }
	manyTokens := make([]string, vectorBatchLimit+1)
	for i := range manyTokens {
		manyTokens[i] = "t"
	}
	token1024 := strings.Repeat("a", 1024)
	token1025 := strings.Repeat("a", 1025)
	const tokenBound = "invalid document vector: token must contain 1..1024 bytes"

	tests := []struct {
		name     string
		err      error
		sentinel error
		want     string
	}{
		{"put generation 0", ValidatePut(0, 1, put("t")), ErrInvalidVector, "invalid document vector: generation, dimension, or batch bound"},
		{"put dimension 0", ValidatePut(1, 0, put("t")), ErrInvalidVector, "invalid document vector: generation, dimension, or batch bound"},
		{"put batch of 1001", ValidatePut(1, 1, put(manyTokens...)), ErrInvalidVector, "invalid document vector: generation, dimension, or batch bound"},
		{"put duplicate token", ValidatePut(1, 1, put("t", "t")), ErrInvalidVector, `invalid document vector: duplicate token "t"`},
		{"put 1024-byte token", ValidatePut(1, 1, put(token1024)), nil, ""},
		{"put empty batch", ValidatePut(1, 1, nil), nil, ""},
		{"put 1025-byte token", ValidatePut(1, 1, put(token1025)), ErrInvalidVector, "embedding 0: " + tokenBound},
		{"put token with NUL", ValidatePut(1, 1, put("a\x00b")), ErrInvalidVector, "embedding 0: " + tokenBound},
		{"put invalid UTF-8", ValidatePut(1, 1, put("a\xffb")), ErrInvalidVector, "embedding 0: " + tokenBound},
		{"put NaN", ValidatePut(1, 1, withVector(float32(math.NaN()))), ErrInvalidVector, `token "t": invalid document vector: non-finite component`},
		{"put +Inf", ValidatePut(1, 1, withVector(float32(math.Inf(1)))), ErrInvalidVector, `token "t": invalid document vector: non-finite component`},
		{"put zero-norm", ValidatePut(1, 1, withVector(0)), ErrInvalidVector, `token "t": invalid document vector: zero-norm vector`},
		{"put wrong dimension", ValidatePut(1, 1, withVector(1, 0)), vector.ErrDimensionMismatch, `dimension mismatch: token "t" has 2 dimensions, want 1`},
		{"tokens generation 0", ValidateTokens(0, []string{"t"}), ErrInvalidVector, "invalid document vector: generation or token batch bound"},
		{"tokens batch of 1001", ValidateTokens(1, manyTokens), ErrInvalidVector, "invalid document vector: generation or token batch bound"},
		{"tokens 1024-byte token", ValidateTokens(1, []string{token1024}), nil, ""},
		{"tokens empty batch", ValidateTokens(1, nil), nil, ""},
		{"tokens 1025-byte token", ValidateTokens(1, []string{token1025}), ErrInvalidVector, tokenBound},
		{"tokens token with NUL", ValidateTokens(1, []string{"a\x00b"}), ErrInvalidVector, tokenBound},
		{"search generation 0", ValidateSearch(0, 1, one, 1), ErrInvalidVector, "invalid document vector: generation, dimension, or result bound"},
		{"search k 1001", ValidateSearch(1, 1, one, vectorBatchLimit+1), ErrInvalidVector, "invalid document vector: generation, dimension, or result bound"},
		{"search wrong dimension", ValidateSearch(1, 2, one, 1), vector.ErrDimensionMismatch, "dimension mismatch: query has 1 dimensions, want 2"},
		{"search zero-norm", ValidateSearch(1, 1, []float32{0}, 1), ErrInvalidVector, "invalid document vector: zero-norm vector"},
		{"search valid", ValidateSearch(1, 1, one, vectorBatchLimit), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.sentinel == nil {
				assert.NoError(t, tt.err)
				return
			}
			require.ErrorIs(t, tt.err, tt.sentinel)
			assert.Equal(t, tt.want, tt.err.Error())
		})
	}
}
