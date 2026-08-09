package embed_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// TestPackDocuments_NeverSplitsDocument catches packers that satisfy a batch
// limit by moving some chunks from one document into a different request.
func TestPackDocuments_NeverSplitsDocument(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	docs := []embed.DocumentInput{
		{Chunks: []string{"aa", "bb"}},
		{Chunks: []string{"cc"}},
	}

	got, err := embed.PackDocuments(docs, embed.RequestLimits{
		MaxDocuments: 2,
		MaxChunks:    2,
		MaxUTF8Bytes: 200,
	})

	require.NoError(err)
	require.Len(got, 2)
	assert.Equal([]embed.DocumentInput{docs[0]}, got[0])
	assert.Equal([]embed.DocumentInput{docs[1]}, got[1])
}

// TestPackDocuments_CountsUTF8BytesAndPromptReserve catches byte accounting
// that counts runes or omits the fixed prompt allowance for every chunk.
func TestPackDocuments_CountsUTF8BytesAndPromptReserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	docs := []embed.DocumentInput{
		{Chunks: []string{"é"}},
		{Chunks: []string{"a"}},
	}

	got, err := embed.PackDocuments(docs, embed.RequestLimits{
		MaxDocuments: 2,
		MaxChunks:    2,
		MaxUTF8Bytes: 130,
	})

	require.NoError(err)
	require.Len(got, 2)
	assert.Equal([]embed.DocumentInput{docs[0]}, got[0])
	assert.Equal([]embed.DocumentInput{docs[1]}, got[1])
}

// TestPackDocuments_RejectsOneDocumentAboveLimit catches packers that send a
// known-oversized document or split its chunks to make it fit.
func TestPackDocuments_RejectsOneDocumentAboveLimit(t *testing.T) {
	docs := []embed.DocumentInput{{Chunks: []string{"aa", "bb"}}}

	got, err := embed.PackDocuments(docs, embed.RequestLimits{
		MaxDocuments: 2,
		MaxChunks:    1,
		MaxUTF8Bytes: 1_000,
	})

	assert.Nil(t, got)
	require.ErrorIs(t, err, embed.ErrDocumentTooLarge)
}

func TestPackDocuments_RejectsInvalidLimits(t *testing.T) {
	_, err := embed.PackDocuments(
		[]embed.DocumentInput{{Chunks: []string{"a"}}},
		embed.RequestLimits{MaxDocuments: 0, MaxChunks: 1, MaxUTF8Bytes: 100},
	)

	require.Error(t, err)
	assert.NotErrorIs(t, err, embed.ErrDocumentTooLarge)
}

// TestPackDocuments_CapsConfiguredDocumentLimit catches positive overrides
// that bypass Voyage's 1,000-document provider ceiling.
func TestPackDocuments_CapsConfiguredDocumentLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	docs := make([]embed.DocumentInput, 1_001)
	for i := range docs {
		docs[i].Chunks = []string{""}
	}

	got, err := embed.PackDocuments(docs, embed.RequestLimits{
		MaxDocuments: 1_001,
		MaxChunks:    2_000,
		MaxUTF8Bytes: 1_000_000,
	})

	require.NoError(err)
	require.Len(got, 2)
	assert.Len(got[0], 1_000)
	assert.Len(got[1], 1)
}

// TestPackDocuments_CapsConfiguredChunkLimit catches positive overrides that
// allow one contextual document above Voyage's 16,000-chunk hard ceiling.
func TestPackDocuments_CapsConfiguredChunkLimit(t *testing.T) {
	got, err := embed.PackDocuments(
		[]embed.DocumentInput{{Chunks: make([]string, 16_001)}},
		embed.RequestLimits{
			MaxDocuments: 1,
			MaxChunks:    16_001,
			MaxUTF8Bytes: 2_000_000,
		},
	)

	assert.Nil(t, got)
	require.ErrorIs(t, err, embed.ErrDocumentTooLarge)
	assert.ErrorContains(t, err, "16000 chunks")
}

// TestPackDocuments_CapsConfiguredUTF8Limit catches positive overrides that
// remove the conservative provider-safe byte ceiling below 120,000 tokens.
func TestPackDocuments_CapsConfiguredUTF8Limit(t *testing.T) {
	got, err := embed.PackDocuments(
		[]embed.DocumentInput{{Chunks: []string{strings.Repeat("x", 100_000)}}},
		embed.RequestLimits{
			MaxDocuments: 1,
			MaxChunks:    1,
			MaxUTF8Bytes: 100_064,
		},
	)

	assert.Nil(t, got)
	assert.ErrorIs(t, err, embed.ErrDocumentTooLarge)
}
