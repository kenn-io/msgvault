package vector

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageVectorIDsDeduplicatesWithoutChangingInput(t *testing.T) {
	in := []int64{9, 7, 8, 7, 9}
	got, err := MessageVectorIDs(Generation{Dimension: 4}, in, 1)
	require.NoError(t, err)
	assert.Equal(t, []int64{7, 8, 9}, got)
	assert.Equal(t, []int64{9, 7, 8, 7, 9}, in)
}

func TestMessageVectorIDsEmptyRequestSkipsValidation(t *testing.T) {
	got, err := MessageVectorIDs(Generation{}, nil, 0)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestMessageVectorIDsRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ids            []int64
		dim, maxChunks int
		wantTooLarge   bool
	}{
		{name: "zero chunks", ids: []int64{1}, dim: 768},
		{name: "negative chunks", ids: []int64{1}, dim: 768, maxChunks: -1},
		{name: "zero dimension", ids: []int64{1}, maxChunks: 4},
		{name: "negative dimension", ids: []int64{1}, dim: -1, maxChunks: 4},
		// The cap counts input IDs, even when deduplication would shrink them.
		{name: "over cap", ids: make([]int64, MaxFilterMessageIDs+1), dim: 768, maxChunks: 4, wantTooLarge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MessageVectorIDs(Generation{Dimension: tc.dim}, tc.ids, tc.maxChunks)
			require.Error(t, err)
			assert.Equal(t, tc.wantTooLarge, errors.Is(err, ErrFilterTooLarge))
		})
	}
}

func TestMessageVectorIDsAcceptsCap(t *testing.T) {
	ids := make([]int64, MaxFilterMessageIDs)
	for i := range ids {
		ids[i] = int64(i)
	}
	got, err := MessageVectorIDs(Generation{Dimension: 4}, ids, 1)
	require.NoError(t, err)
	assert.Len(t, got, MaxFilterMessageIDs)
}
