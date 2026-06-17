package embed

import (
	"strings"
	"testing"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestChunkText(t *testing.T) {
	t.Run("EmptyInputReturnsNil", func(t *testing.T) {
		got, _ := ChunkText("", 100, 10, 0)
		assertpkg.Nil(t, got)
	})

	t.Run("ShortInputReturnsSingleSpan", func(t *testing.T) {
		assert := assertpkg.New(t)
		got, _ := ChunkText("hello world", 100, 10, 0)
		requirepkg.Len(t, got, 1)
		assert.Equal("hello world", got[0].Text)
		assert.Equal(0, got[0].CharStart)
		assert.Equal(11, got[0].CharEnd)
	})

	t.Run("MaxRunesZeroDisablesChunking", func(t *testing.T) {
		text := strings.Repeat("x", 1000)
		got, _ := ChunkText(text, 0, 10, 0)
		requirepkg.Len(t, got, 1, "expected single span covering whole text")
		assertpkg.Equal(t, text, got[0].Text)
	})

	t.Run("CutsAtParagraphBreakInBackQuarter", func(t *testing.T) {
		// Build 100 chars of text where a "\n\n" sits at offset 80.
		// The back quarter of a 100-rune window starts at offset 75,
		// so the cut should land at the paragraph break (offset 82,
		// right after "\n\n").
		first := strings.Repeat("a", 80)
		second := strings.Repeat("b", 50)
		text := first + "\n\n" + second
		got, _ := ChunkText(text, 100, 10, 0)
		requirepkg.GreaterOrEqual(t, len(got), 2, "expected >= 2 chunks")
		assertpkg.Equal(t, 82, got[0].CharEnd, "first chunk ends at 82 (right after \\n\\n)")
		assertpkg.True(t, strings.HasSuffix(got[0].Text, "\n\n"),
			"first chunk should end with paragraph break; got %q", got[0].Text[len(got[0].Text)-5:])
	})

	t.Run("CutsAtSentenceBoundaryWhenNoParagraph", func(t *testing.T) {
		first := strings.Repeat("a", 80)
		// Sentence terminator at offset 80 ("end. ").
		text := first + ". " + strings.Repeat("b", 50)
		got, _ := ChunkText(text, 100, 10, 0)
		requirepkg.GreaterOrEqual(t, len(got), 2, "expected >= 2 chunks")
		// findSoftBreak returns the index *after* ". " (so 82).
		assertpkg.Equal(t, 82, got[0].CharEnd)
	})

	t.Run("CutsAtWordBoundaryWhenNoSentence", func(t *testing.T) {
		// Construct a 100-rune window where the back quarter has only
		// a space (no sentence terminator). Cut should land at the
		// last space inside [75, 100).
		text := strings.Repeat("a", 90) + " " + strings.Repeat("b", 50)
		got, _ := ChunkText(text, 100, 10, 0)
		requirepkg.GreaterOrEqual(t, len(got), 2, "expected >= 2 chunks")
		assertpkg.Equal(t, 91, got[0].CharEnd, "one past space at 90")
	})

	t.Run("HardCutsWhenNoSoftBreakInBackQuarter", func(t *testing.T) {
		// 1000 unbroken non-space chars; no soft break anywhere. Each
		// window should land on the hard cut at maxRunes.
		text := strings.Repeat("a", 1000)
		got, _ := ChunkText(text, 100, 0, 0)
		requirepkg.Len(t, got, 10)
		for i, s := range got {
			assertpkg.Equal(t, 100, s.CharEnd-s.CharStart, "chunk %d", i)
		}
	})

	t.Run("OverlapBetweenConsecutiveChunks", func(t *testing.T) {
		text := strings.Repeat("a", 300)
		got, _ := ChunkText(text, 100, 20, 0)
		requirepkg.GreaterOrEqual(t, len(got), 2, "expected >= 2 chunks")
		// With overlap=20, the second chunk should start 80 runes
		// after the first chunk's start.
		assertpkg.Equal(t, 80, got[1].CharStart)
	})

	t.Run("OverlapClampedToHalfWindow", func(t *testing.T) {
		// overlap >= maxRunes would mean cursor never advances —
		// the function must clamp it. With maxRunes=100 and
		// overlap=500, effective overlap should be 50.
		text := strings.Repeat("a", 300)
		got, _ := ChunkText(text, 100, 500, 0)
		requirepkg.NotEmpty(t, got, "got no chunks (overlap not clamped → infinite loop)")
		// With effective overlap=50, chunk[1] starts at 50.
		if len(got) >= 2 {
			assertpkg.Equal(t, 50, got[1].CharStart)
		}
	})

	t.Run("AllSpansHaveValidUTF8AndCorrectText", func(t *testing.T) {
		assert := assertpkg.New(t)
		// Mixed-script input with multi-byte runes scattered through.
		var b strings.Builder
		for range 50 {
			b.WriteString("Hello world. ")
			b.WriteString("こんにちは世界。")
		}
		text := b.String()
		got, _ := ChunkText(text, 80, 10, 0)
		requirepkg.GreaterOrEqual(t, len(got), 2, "expected >= 2 chunks")
		for i, s := range got {
			assert.True(utf8.ValidString(s.Text), "chunk %d: invalid UTF-8 in span text", i)
			// Span text must match the substring derived from the
			// CharStart/CharEnd offsets — guards against off-by-one in
			// the byte/rune translation.
			runes := []rune(text)
			if s.CharStart < 0 || s.CharEnd > len(runes) || s.CharStart >= s.CharEnd {
				assert.Failf("invalid span", "chunk %d: [%d, %d)", i, s.CharStart, s.CharEnd)
				continue
			}
			expect := string(runes[s.CharStart:s.CharEnd])
			assert.Equal(expect, s.Text, "chunk %d: Text != runes[Start:End]", i)
		}
	})

	t.Run("MaxSpansCapsInputBytesProcessed", func(t *testing.T) {
		// Roborev regression on e83967b: a multi-megabyte body must
		// not cause the chunker to walk the entire input before
		// maxSpans takes effect. With maxSpans=3 and maxRunes=100,
		// the chunker should look at at most ~300 runes regardless
		// of how long the input is. Use a 10M-rune input; if
		// buildRuneByteOffsets ever ran over the whole thing, this
		// test would dominate the suite at ~100ms+; with the early
		// cap it stays at the same cost as the 1K-rune cases below.
		text := strings.Repeat("a", 10_000_000)
		got, _ := ChunkText(text, 100, 0, 3)
		requirepkg.Len(t, got, 3, "input cap kicks in before allocating offsets for 10M runes")
		// All emitted spans come from the head of the input.
		for i, s := range got {
			assertpkg.LessOrEqual(t, s.CharEnd, 300, "chunk %d should end at most at 300 (the cap window)", i)
		}
	})

	t.Run("MaxSpansCapsOutputAndDropsTail", func(t *testing.T) {
		// A pathological input — 10× window with no soft breaks — would
		// normally chunk into 10 spans. With maxSpans=3 we get exactly 3
		// covering the head, and the tail beyond the third chunk's end
		// is dropped on the floor (real semantic search doesn't gain
		// from embedding system-generated dumps).
		text := strings.Repeat("a", 1000)
		got, _ := ChunkText(text, 100, 0, 3)
		requirepkg.Len(t, got, 3, "capped")
		assertpkg.Equal(t, 0, got[0].CharStart, "first chunk should start at 0")
		last := got[len(got)-1]
		assertpkg.Less(t, last.CharEnd, 1000, "expected dropped tail beyond last capped chunk")
	})

	t.Run("TailDroppedFlagsCapWhenLastChunkLandsOnSoftBreak", func(t *testing.T) {
		// Roborev regression: when maxSpans caps the output but the
		// last emitted chunk happens to land on a clean soft break
		// (so the per-chunk hard-cut Trunc signal stays false), the
		// caller would otherwise have no way to know the message
		// lost tail content. The returned tailDropped bool surfaces
		// the cap so downstream metrics record the truncation.
		var b strings.Builder
		// 5 chunks worth of prose, each ending neatly with "X. " so
		// findSoftBreak returns a sentence boundary near windowEnd
		// and Trunc stays false on every chunk. With maxSpans=2 we
		// emit 2 chunks and drop the rest — tailDropped must be true.
		for range 5 {
			b.WriteString(strings.Repeat("a", 85))
			b.WriteString(". ")
		}
		text := b.String()
		got, tailDropped := ChunkText(text, 90, 0, 2)
		requirepkg.Len(t, got, 2, "maxSpans cap")
		assertpkg.True(t, tailDropped,
			"tailDropped should be true (maxSpans dropped %d runes past chunk[1].CharEnd=%d)",
			utf8.RuneCountInString(text)-got[1].CharEnd, got[1].CharEnd)
	})

	t.Run("TailDroppedFalseWhenAllContentEmitted", func(t *testing.T) {
		// Counter-test for the above: a short input that fits in
		// fewer than maxSpans chunks must not flag tailDropped.
		text := strings.Repeat("a", 150)
		got, tailDropped := ChunkText(text, 100, 0, 10)
		requirepkg.NotEmpty(t, got, "expected >= 1 chunk")
		assertpkg.False(t, tailDropped, "short input that fit in %d chunks should not flag tailDropped", len(got))
	})

	t.Run("MaxSpansZeroIsUnlimited", func(t *testing.T) {
		text := strings.Repeat("a", 1000)
		got, _ := ChunkText(text, 100, 0, 0)
		assertpkg.Len(t, got, 10, "no cap")
	})

	t.Run("MaxSpansLargerThanNaturalChunkCountIsNoop", func(t *testing.T) {
		text := strings.Repeat("a", 300)
		got, _ := ChunkText(text, 100, 0, 100)
		assertpkg.Len(t, got, 3, "cap above natural")
	})

	t.Run("ConcatenationCoversInputModuloOverlap", func(t *testing.T) {
		assert := assertpkg.New(t)
		// Stitching the chunks back together (advancing by stride =
		// window - overlap from each chunk's start) must reconstruct
		// the input verbatim. This is the property the overlap
		// guarantee depends on for recall.
		text := strings.Repeat("Lorem ipsum dolor sit amet. ", 200)
		spans, _ := ChunkText(text, 200, 30, 0)
		requirepkg.GreaterOrEqual(t, len(spans), 2, "need >= 2 chunks to test stitching")
		// Each chunk starts at spans[i].CharStart; the unique part of
		// chunk i (not seen in chunk i-1) starts at spans[i].CharStart
		// + overlap-with-prev. For correctness it's enough to verify
		// spans cover [0, totalRunes) end-to-end.
		assert.Equal(0, spans[0].CharStart, "first chunk should start at 0")
		last := spans[len(spans)-1]
		runeCount := utf8.RuneCountInString(text)
		assert.Equal(runeCount, last.CharEnd, "last chunk should end at end of text")
		// Every gap between consecutive chunks must be <= window so
		// no input runes are dropped on the floor.
		for i := 1; i < len(spans); i++ {
			assert.LessOrEqualf(spans[i].CharStart, spans[i-1].CharEnd,
				"chunks %d,%d leave a gap [%d, %d)",
				i-1, i, spans[i-1].CharEnd, spans[i].CharStart)
		}
	})
}
