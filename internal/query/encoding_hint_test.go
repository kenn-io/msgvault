package query

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsEncodingError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"unrelated error", errors.New("connection refused"), false},
		{"encoding error", errors.New("Invalid string encoding found in Parquet file"), true},
		{"wrapped encoding error", fmt.Errorf("aggregate query: %w", errors.New("Invalid string encoding found in Parquet file")), true},
		{"encoding error substring", errors.New("scan: Invalid string encoding found in Parquet file foo.parquet"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsEncodingError(tt.err))
		})
	}
}

func TestHintRepairEncoding(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		assert.NoError(t, HintRepairEncoding(nil))
	})

	t.Run("unrelated error passes through", func(t *testing.T) {
		orig := errors.New("something else")
		got := HintRepairEncoding(orig)
		assert.Same(t, orig, got, "HintRepairEncoding should return original error unchanged")
	})

	t.Run("encoding error gets hint", func(t *testing.T) {
		orig := errors.New("Invalid string encoding found in Parquet file")
		got := HintRepairEncoding(orig)
		require.Error(t, got, "HintRepairEncoding returned nil")
		assert.Contains(t, got.Error(), "repair-encoding")
		// Original error should be preserved in the chain
		assert.ErrorIs(t, got, orig, "wrapped error should preserve original via errors.Is")
	})

	t.Run("wrapped encoding error gets hint", func(t *testing.T) {
		inner := errors.New("Invalid string encoding found in Parquet file")
		wrapped := fmt.Errorf("aggregate query: %w", inner)
		got := HintRepairEncoding(wrapped)
		assert.Contains(t, got.Error(), "repair-encoding")
	})
}
