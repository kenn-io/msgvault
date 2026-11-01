package embed

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/embedmodel"
)

// TestDropBlankChunksMatchesEncoderRule pins the assemblers to the same rule
// the scan worker already applies. strings.TrimSpace passes text made only of
// invisible formatting, which the encoder then rejects for the whole batch.
func TestDropBlankChunksMatchesEncoderRule(t *testing.T) {
	assert := assert.New(t)
	blank := []string{"", "  \t\n ", "\u200b\u200b", "\u00ad", "\u200e\u200f", "\x00\x01"}
	for _, text := range blank {
		assert.True(embedmodel.BlankText(text), "%q must count as blank", text)
		assert.Empty(dropBlankChunks([]OwnedChunk{{MessageID: 1, Text: text}}),
			"%q must be dropped before it reaches the encoder", text)
	}
	for _, text := range []string{"hello", "\u200bhello\u200b", "a"} {
		assert.False(embedmodel.BlankText(text), "%q carries content", text)
		assert.Len(dropBlankChunks([]OwnedChunk{{MessageID: 1, Text: text}}), 1,
			"%q must be kept", text)
	}
}

// TestDropBlankChunksRenumbersPerMessage checks ChunkIndex stays contiguous
// within each message. A chat window carries chunks from several messages, so
// renumbering across the document would shift every later message's indices.
func TestDropBlankChunksRenumbersPerMessage(t *testing.T) {
	assert := assert.New(t)
	got := dropBlankChunks([]OwnedChunk{
		{MessageID: 1, ChunkIndex: 0, Text: "a1"},
		{MessageID: 1, ChunkIndex: 1, Text: "\u200b"},
		{MessageID: 1, ChunkIndex: 2, Text: "a2"},
		{MessageID: 2, ChunkIndex: 0, Text: "b1"},
		{MessageID: 2, ChunkIndex: 1, Text: "b2"},
	})
	type pair struct {
		id    int64
		index int
	}
	var seen []pair
	for _, chunk := range got {
		seen = append(seen, pair{chunk.MessageID, chunk.ChunkIndex})
	}
	assert.Equal([]pair{{1, 0}, {1, 1}, {2, 0}, {2, 1}}, seen)
}

// TestAssembleOrdinaryDocumentSkipsBlankBody checks a message whose assembled
// text is only invisible characters produces no document, rather than one the
// encoder refuses. A subject is deliberately absent: Preprocess renders one as
// a literal "Subject:" line, which is real content and must still be embedded.
func TestAssembleOrdinaryDocumentSkipsBlankBody(t *testing.T) {
	assert := assert.New(t)
	_, ok := assembleOrdinaryDocument(
		AssemblyMessage{ID: 42, Body: "\u200e\u00ad\u200b"},
		AssemblyPolicy{MaxChunkRunes: 100})
	assert.False(ok, "a document of only invisible runes must be skipped")

	_, ok = assembleOrdinaryDocument(
		AssemblyMessage{ID: 43, Subject: "\u200b", Body: "\u200e\u00ad"},
		AssemblyPolicy{MaxChunkRunes: 100})
	assert.True(ok, "a rendered Subject line is real content and still embeds")
}
