package embed_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// TestDocument_PreservesOwnershipMetadata catches document boundaries that
// cannot carry the source and metadata versions needed for invalidation.
func TestDocument_PreservesOwnershipMetadata(t *testing.T) {
	assert := assert.New(t)
	lastModified := time.Date(2026, 8, 8, 12, 30, 0, 0, time.UTC)
	doc := embed.Document{
		Key:            "conversation:42",
		Kind:           "conversation",
		ScopeKey:       "conversation:42",
		Revision:       "revision-7",
		SourceSequence: 11,
		Versions: []embed.SourceVersion{{
			MessageID:    101,
			LastModified: lastModified,
		}},
		MetadataVersion: embed.MetadataVersion{
			ConversationID: 42,
			Digest:         "metadata-digest",
		},
		Chunks: []embed.OwnedChunk{{
			MessageID:       101,
			ChunkIndex:      2,
			Text:            "contextual body chunk",
			SourceCharStart: 12,
			SourceCharEnd:   33,
			SourceBasis:     vector.SourceBasisBody,
			Truncated:       true,
		}},
	}

	assert.Equal("conversation:42", doc.Key)
	assert.Equal(int64(11), doc.SourceSequence)
	assert.Equal(lastModified, doc.Versions[0].LastModified)
	assert.Equal(int64(42), doc.MetadataVersion.ConversationID)
	assert.Equal(2, doc.Chunks[0].ChunkIndex)
	assert.Equal(vector.SourceBasisBody, doc.Chunks[0].SourceBasis)
}

// TestDocument_AffectedScopePreservesSelector catches invalidation scopes
// that drop a conversation, time window, or single-message selector.
func TestDocument_AffectedScopePreservesSelector(t *testing.T) {
	assert := assert.New(t)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	scope := embed.AffectedScope{
		Kind:           "conversation-window",
		ConversationID: 42,
		UTCStart:       start,
		UTCEnd:         end,
		MessageID:      101,
		Undated:        true,
	}

	assert.Equal("conversation-window", scope.Kind)
	assert.Equal(int64(42), scope.ConversationID)
	assert.Equal(start, scope.UTCStart)
	assert.Equal(end, scope.UTCEnd)
	assert.Equal(int64(101), scope.MessageID)
	assert.True(scope.Undated)
}

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
