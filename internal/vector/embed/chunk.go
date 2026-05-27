package embed

import (
	"strings"
	"unicode/utf8"
)

// ChunkSpan describes one window into the preprocessed source text.
// CharStart / CharEnd are rune offsets (not byte offsets) into the
// input string passed to ChunkText, so a backend can recover the exact
// substring later for highlighting. Text is that substring, already
// extracted; callers feed Text directly to the embedder.
type ChunkSpan struct {
	Text      string
	CharStart int
	CharEnd   int
}

// ChunkText splits text into windows of at most maxRunes runes each,
// overlapping by overlapRunes runes between consecutive windows. The
// goal is twofold:
//
//  1. Every span fits inside the embedder's context window. A message
//     longer than maxRunes that would otherwise be truncated (and lose
//     its tail) instead produces multiple spans, each individually
//     embedded.
//  2. A query term that lives near a window boundary can still match.
//     The overlap means a sentence straddling the cut shows up
//     verbatim in both spans, so the ANN search retrieves whichever
//     span scores higher.
//
// When the input fits, ChunkText returns a single span covering the
// whole text. When it doesn't, ChunkText prefers to cut at the last
// paragraph break (\n\n) inside the final 25% of the window, falling
// back to a sentence terminator (". "/"! "/"? "/"\n"), then to a word
// boundary (space), then to a hard rune cut. This avoids slicing the
// embedder's input mid-word, which materially helps recall on prose.
//
// maxRunes <= 0 disables chunking and returns a single span. The
// overlap is clamped so it never exceeds maxRunes/2 — an overlap as
// large as the window itself would loop forever.
//
// maxSpans caps the number of returned spans, dropping tail content
// from any message that would otherwise produce more chunks. <= 0
// disables the cap. The cap exists for a real-world failure mode:
// system-generated error dumps and stack traces can exceed 10 MB of
// body text and would chunk into thousands of spans, blowing past the
// embedder's batch-size assumptions and pushing the whole batch over
// the API timeout. Most callers should pass a generous cap (50–100)
// — enough to cover any legitimate long-form email while keeping
// pathological inputs bounded.
//
// The returned tailDropped is true when ChunkText dropped content past
// the last emitted span — either because the input was first truncated
// by the maxSpans*maxRunes pre-cap, or because the maxSpans cap fired
// inside the main loop while more chunks would otherwise have been
// emitted. Callers use this to mark the message as truncated for
// downstream metrics, regardless of whether the last emitted chunk
// happened to land on a clean soft break (in which case the
// per-chunk Trunc-on-hard-cut signal would not fire).
func ChunkText(text string, maxRunes, overlapRunes, maxSpans int) (spans []ChunkSpan, tailDropped bool) {
	if text == "" {
		return nil, false
	}
	if maxRunes <= 0 {
		return []ChunkSpan{{Text: text, CharStart: 0, CharEnd: utf8.RuneCountInString(text)}}, false
	}

	// Cap text early. Without this, a sync that lands a multi-megabyte
	// body forces buildRuneByteOffsets to allocate ~8 bytes per rune
	// for the whole input — a 15 MB email becomes a ~120 MB allocation
	// even though only the first maxSpans*maxRunes worth of content
	// can possibly be emitted. The chunker only ever reads ahead
	// within a window of maxRunes runes, so this cap is lossless for
	// the spans it would otherwise emit; the tail beyond the cap
	// would have been dropped on the floor by the maxSpans guard
	// inside the loop anyway.
	if maxSpans > 0 {
		keep := maxSpans * maxRunes
		walked := 0
		for i := range text {
			if walked >= keep {
				text = text[:i]
				tailDropped = true
				break
			}
			walked++
		}
	}

	totalRunes := utf8.RuneCountInString(text)
	if totalRunes <= maxRunes {
		return []ChunkSpan{{Text: text, CharStart: 0, CharEnd: totalRunes}}, tailDropped
	}
	if overlapRunes < 0 {
		overlapRunes = 0
	}
	if overlapRunes >= maxRunes {
		overlapRunes = maxRunes / 2
	}

	// Pre-compute byte offsets for every rune so each window slice is
	// O(window) rather than O(text). Slice indices live in rune space;
	// byte conversion happens once via runeByteOffsets[i].
	runeByteOffsets := buildRuneByteOffsets(text)
	// runeByteOffsets has one entry per rune plus a sentinel for the
	// trailing position, so len-1 == totalRunes.
	if len(runeByteOffsets)-1 != totalRunes {
		// Defensive: out-of-band UTF-8 should never reach here, but
		// fall back to a single span rather than panic on a bad input.
		return []ChunkSpan{{Text: text, CharStart: 0, CharEnd: totalRunes}}, tailDropped
	}

	cursor := 0
	for cursor < totalRunes {
		if maxSpans > 0 && len(spans) >= maxSpans {
			// Hit the per-message cap. The tail of the message is
			// dropped; the spans collected so far already cover the
			// head, which is what semantic search cares about. We
			// could synthesize a final "remaining content omitted"
			// span here, but that would just feed the embedder a
			// content-free string. Flag tailDropped so the caller
			// knows the message wasn't fully covered, regardless of
			// where the last emitted chunk landed (the per-chunk
			// Trunc flag only fires on hard cuts).
			tailDropped = true
			break
		}
		windowEnd := min(cursor+maxRunes, totalRunes)
		// Look for a soft break in the back quarter of the window;
		// if found, cut there instead of the hard end. Skip this when
		// the window already reaches the end of text — no need to find
		// a "nice" cut for the final span.
		cut := windowEnd
		if windowEnd < totalRunes {
			searchFloor := max(cursor+(maxRunes*3/4), cursor+1)
			cut = findSoftBreak(text, runeByteOffsets, searchFloor, windowEnd)
		}

		startByte := runeByteOffsets[cursor]
		endByte := runeByteOffsets[cut]
		span := ChunkSpan{
			Text:      text[startByte:endByte],
			CharStart: cursor,
			CharEnd:   cut,
		}
		spans = append(spans, span)

		if cut >= totalRunes {
			break
		}
		// Advance the cursor by (window - overlap), but never less
		// than 1 rune — guards against pathological inputs where the
		// soft break sits inside the overlap zone.
		advance := max((cut-cursor)-overlapRunes, 1)
		cursor += advance
	}
	return spans, tailDropped
}

// buildRuneByteOffsets returns a slice of length runeCount+1 where
// entry i is the byte offset of rune i (and the final entry is len(s)).
// Used by ChunkText to map rune-space slice indices back to byte slices
// without walking the string twice per window.
func buildRuneByteOffsets(s string) []int {
	out := make([]int, 0, len(s)/2+1)
	for i := range s {
		out = append(out, i)
	}
	out = append(out, len(s))
	return out
}

// findSoftBreak scans runeByteOffsets[floor:ceil] for the last
// preferred break — paragraph, sentence, or word — and returns its
// rune index. Returns ceil if no soft break is found in the search
// window. Ordering: paragraph beats sentence beats word, so a "good"
// boundary near the end wins over a "great" one near the beginning of
// the search window. This keeps each chunk closer to maxRunes; cutting
// far short to chase a paragraph break would waste budget.
func findSoftBreak(text string, runeByteOffsets []int, floor, ceil int) int {
	// Examine bytes between floor and ceil. We compare against fixed
	// ASCII byte patterns — none of the break sequences span multi-byte
	// runes, so byte-level lookups give the same answer as rune-level
	// without the per-iteration UTF-8 decode.
	byteFloor := runeByteOffsets[floor]
	byteCeil := runeByteOffsets[ceil]
	window := text[byteFloor:byteCeil]

	paragraph := strings.LastIndex(window, "\n\n")
	if paragraph >= 0 {
		// Cut after the "\n\n" so the new chunk does not begin with
		// blank lines.
		return byteToRune(runeByteOffsets, byteFloor+paragraph+2)
	}

	// Sentence terminators followed by space/newline. Check ". " /
	// "? " / "! " / ".\n" / "?\n" / "!\n".
	bestSentence := -1
	for _, term := range []string{". ", "? ", "! ", ".\n", "?\n", "!\n"} {
		if idx := strings.LastIndex(window, term); idx > bestSentence {
			bestSentence = idx + len(term)
		}
	}
	if bestSentence > 0 {
		return byteToRune(runeByteOffsets, byteFloor+bestSentence)
	}

	// Word boundary — last space in the window.
	if space := strings.LastIndexByte(window, ' '); space > 0 {
		return byteToRune(runeByteOffsets, byteFloor+space+1)
	}

	return ceil
}

// byteToRune converts a byte offset into the rune index by binary
// searching runeByteOffsets. Faster than a linear scan when the offset
// is far from either end of the string.
func byteToRune(runeByteOffsets []int, b int) int {
	lo, hi := 0, len(runeByteOffsets)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if runeByteOffsets[mid] < b {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
