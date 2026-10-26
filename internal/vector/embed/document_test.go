package embed_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// TestDocument_SourceBasisZeroValuePreservesSubjectBody catches a new enum
// whose zero value silently changes legacy chunks to contextual body offsets.
func TestDocument_SourceBasisZeroValuePreservesSubjectBody(t *testing.T) {
	var chunk vector.Chunk
	var hit vector.ChunkHit

	assert.Equal(t, vector.SourceBasisSubjectBody, chunk.SourceBasis)
	assert.Equal(t, vector.SourceBasisSubjectBody, hit.SourceBasis)
	assert.NotEqual(t, vector.SourceBasisSubjectBody, vector.SourceBasisBody)
}

var _ embed.SemanticClient = (*embed.Client)(nil)
