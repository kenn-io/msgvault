package documentindex

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeDocumentPreservesEvidenceAndRemovesActiveContent(t *testing.T) {
	assert := assert.New(t)
	markdown := `# Damage report

The carton was **crushed**. [Safe](https://example.test/report?id=42)
and [unsafe](javascript:alert(1)).

![Photo evidence](data:image/png;base64,AAAA)

| Item | Count |
| --- | ---: |
| Carton | 3 |
| Pallet | 1 |

<script>private_executable_marker()</script>
<style>.hidden { display: none }</style>

## Inspection

- First edge
- Second edge

~~~go
package synthetic
~~~
`
	source := SourceDocument{Family: "pdf", UnitKind: "page", Units: []SourceUnit{{
		Index: 0, Markdown: markdown, Header: "Warehouse **A**", Footer: "Page 1",
		Dimensions: UnitDimensions{DPI: 200, Height: 2200, Width: 1700},
	}}}
	policy := DefaultNormalizePolicy(100_000)
	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Units, 1)
	unit := normalized.Units[0]
	assert.Contains(unit.Text, "# Damage report")
	assert.Contains(unit.Text, "carton was crushed")
	assert.Contains(unit.Text, "Safe (https://example.test/report?id=42)")
	assert.Contains(unit.Text, "unsafe")
	assert.NotContains(unit.Text, "javascript:")
	assert.Contains(unit.Text, "Photo evidence")
	assert.NotContains(unit.Text, "data:image")
	assert.Contains(unit.Text, "Item | Count")
	assert.Contains(unit.Text, "Carton | 3")
	assert.Contains(unit.Text, "Pallet | 1")
	assert.NotContains(unit.Text, "private_executable_marker")
	assert.NotContains(unit.Text, "display: none")
	assert.Contains(unit.Text, "```go\npackage synthetic\n```")
	assert.Contains(unit.Text, "Warehouse A")
	assert.Contains(unit.Text, "Page 1")
	assert.Equal("Warehouse A", unit.Header)
	assert.Equal("Page 1", unit.Footer)
	assert.Regexp(`^[0-9a-f]{64}$`, unit.Checksum)
	assert.Regexp(`^[0-9a-f]{64}$`, normalized.Checksum)
}

func TestNormalizeDocumentPublishesHeaderAndFooterOnlyEvidence(t *testing.T) {
	source := SourceDocument{Family: "pdf", UnitKind: "page", Units: []SourceUnit{{
		Index: 0, Header: "Confidential **shipment**", Footer: "Dock 7",
	}}}

	normalized, err := NormalizeDocument(source, DefaultNormalizePolicy(100_000))
	require.NoError(t, err)
	require.Len(t, normalized.Units, 1)
	assert.Equal(t, "Confidential shipment\n\nDock 7", normalized.Units[0].Text)
	assert.Equal(t, "Confidential shipment", normalized.Units[0].Header)
	assert.Equal(t, "Dock 7", normalized.Units[0].Footer)
	require.Len(t, normalized.Chunks, 1)
	assert.Equal(t, normalized.Units[0].Text, normalized.Chunks[0].Text)
	assert.Equal(t, 0, normalized.Chunks[0].Spans[0].CharStart)
	assert.Equal(t, normalized.Units[0].CharCount, normalized.Chunks[0].Spans[0].CharEnd)
}

func TestNormalizeDocumentChunksHaveExactUnitSpansAndHeadingPaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	markdown := "# Alpha\n\n" + strings.Repeat("alpha evidence sentence. ", 8) +
		"\n\n## Beta\n\n" + strings.Repeat("beta evidence sentence. ", 8)
	policy := DefaultNormalizePolicy(10_000)
	policy.MaxChunkRunes = 100
	policy.ChunkOverlap = 10
	source := SourceDocument{Family: "word", UnitKind: "page", Units: []SourceUnit{{Index: 0, Markdown: markdown}}}

	first, err := NormalizeDocument(source, policy)
	require.NoError(err)
	second, err := NormalizeDocument(source, policy)
	require.NoError(err)
	assert.Equal(first, second)
	require.Greater(len(first.Chunks), 1)

	runes := []rune(first.Units[0].Text)
	for ordinal, chunk := range first.Chunks {
		assert.Equal(ordinal, chunk.Ordinal)
		require.Len(chunk.Spans, 1)
		span := chunk.Spans[0]
		assert.Equal(chunk.Text, string(runes[span.CharStart:span.CharEnd]))
		assert.Equal(utf8.RuneCountInString(chunk.Text), chunk.CharCount)
		assert.Regexp(`^[0-9a-f]{64}$`, chunk.Checksum)
	}
	assert.Equal([]string{"Alpha"}, first.Chunks[0].HeadingPath)
	assert.Contains(first.Chunks[len(first.Chunks)-1].HeadingPath, "Beta")
}

func TestNormalizeDocumentBoundsUnicodeAndRejectsImpossibleUnits(t *testing.T) {
	assert := assert.New(t)
	policy := DefaultNormalizePolicy(6)
	policy.MaxUnitChars = 6
	policy.MaxChunkRunes = 3
	policy.ChunkOverlap = 1
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{Index: 0, Markdown: "é日🙂abcdef"}}}
	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Equal("é日🙂abc", normalized.Units[0].Text)
	assert.True(normalized.Units[0].Truncated)
	assert.True(normalized.Truncated)
	assert.True(utf8.ValidString(normalized.Units[0].Text))

	source.Units = append(source.Units, SourceUnit{Index: 3, Markdown: "later"})
	_, err = NormalizeDocument(source, policy)
	require.ErrorContains(t, err, "noncontiguous index")

	source.Units = []SourceUnit{{Index: 0, Markdown: "x", Dimensions: UnitDimensions{Width: -1}}}
	_, err = NormalizeDocument(source, policy)
	require.ErrorContains(t, err, "invalid dimensions")
}

func TestNormalizeDocumentCapsChunksWithoutInventingSpans(t *testing.T) {
	assert := assert.New(t)
	policy := DefaultNormalizePolicy(100_000)
	policy.MaxChunkRunes = 20
	policy.ChunkOverlap = 2
	policy.MaxChunks = 2
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{
		Index: 0, Markdown: strings.Repeat("bounded evidence ", 20),
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Len(normalized.Chunks, 2)
	assert.True(normalized.Truncated)
	for _, chunk := range normalized.Chunks {
		assert.NotEmpty(chunk.Text)
		assert.Len(chunk.Spans, 1)
	}
}

func TestNormalizeDocumentBoundsHeadingProvenanceToRetainedText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	policy := DefaultNormalizePolicy(12)
	policy.MaxUnitChars = 12
	policy.MaxChunkRunes = 12
	policy.ChunkOverlap = 2
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{
		Index: 0, Markdown: "# retained-heading-content-that-is-cut\n\nbody",
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(err)
	require.Len(normalized.Units, 1)
	unit := normalized.Units[0]
	require.Len(unit.HeadingMarks, 1)
	assert.Equal("# retained-h", unit.Text)
	assert.Equal([]string{"retained-h"}, unit.HeadingMarks[0].Path)
	assert.NotContains(unit.HeadingMarks[0].Path[0], "content")
}

func TestNormalizeDocumentPreservesInlineAdjacencyAndTaskState(t *testing.T) {
	assert := assert.New(t)
	policy := DefaultNormalizePolicy(10_000)
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{
		Index:    0,
		Markdown: "pre**condition**ed and `identifier`\n\n- [x] shipped\n- [ ] pending",
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	text := normalized.Units[0].Text
	assert.Contains(text, "preconditioned")
	assert.NotContains(text, "pre condition ed")
	assert.Contains(text, "- [x] shipped")
	assert.Contains(text, "- [ ] pending")
}

func TestNormalizeDocumentHeadingMetadataIgnoresCodeAndBoundsSource(t *testing.T) {
	assert := assert.New(t)
	policy := DefaultNormalizePolicy(10_000)
	policy.MaxSourceUnitBytes = 120
	source := SourceDocument{Family: "source", UnitKind: "section", Units: []SourceUnit{{
		Index:    0,
		Markdown: "# Real\n\n```sh\n# not a heading\necho safe\n```\n\n" + strings.Repeat("bounded tail ", 20),
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Units, 1)
	unit := normalized.Units[0]
	assert.True(unit.Truncated)
	assert.True(normalized.Truncated)
	require.Len(t, unit.HeadingMarks, 1)
	assert.Equal([]string{"Real"}, unit.HeadingMarks[0].Path)
	for _, chunk := range normalized.Chunks {
		assert.NotContains(chunk.HeadingPath, "not a heading")
	}
}
