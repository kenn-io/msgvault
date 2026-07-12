package mcp

import (
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/vector/chunkmatch"
)

// messageMatch is the unified excerpt shape for search_in_message and
// search_message_bodies (keyword, vector, and hybrid). Keyword matches always
// carry a raw-body char_offset and line. Vector locations are omitted when
// preprocessing prevents an exact raw-body mapping. score is set for vector
// chunk matches only.
type messageMatch struct {
	CharOffset *int     `json:"char_offset,omitempty"`
	Snippet    string   `json:"snippet"`
	Line       *int     `json:"line,omitempty"`
	Score      *float64 `json:"score,omitempty"`
}

func messageMatchesFromChunks(matches []chunkmatch.Match) []messageMatch {
	if len(matches) == 0 {
		return nil
	}
	out := make([]messageMatch, len(matches))
	for i, match := range matches {
		score := match.Score
		out[i] = messageMatch{
			CharOffset: match.CharOffset,
			Snippet:    match.Snippet,
			Line:       match.Line,
			Score:      &score,
		}
	}
	return out
}

// extractContextChar returns up to contextChars of body text centered on each
// case-insensitive term match, merging overlapping windows.
func extractContextChar(body string, terms []string, contextChars int) []string {
	if body == "" || len(terms) == 0 || contextChars <= 0 {
		return nil
	}

	lowerBody := strings.ToLower(body)

	type span struct {
		start, end int
	}
	var spans []span

	for _, term := range terms {
		if len(term) < 2 {
			continue
		}
		lowerTerm := strings.ToLower(term)
		termLen := len(term)
		searchFrom := 0
		for {
			idx := strings.Index(lowerBody[searchFrom:], lowerTerm)
			if idx < 0 {
				break
			}
			pos := searchFrom + idx
			searchFrom = pos + 1

			start, end := contextWindow(len(body), pos, termLen, contextChars)
			spans = append(spans, span{start: start, end: end})
		}
	}

	if len(spans) == 0 {
		return nil
	}

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end < spans[j].end
		}
		return spans[i].start < spans[j].start
	})

	merged := []span{spans[0]}
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			last.end = max(last.end, s.end)
			continue
		}
		merged = append(merged, s)
	}

	out := make([]string, 0, len(merged))
	for _, s := range merged {
		out = append(out, bodyByteSlice(body, s.start, s.end))
	}
	return out
}

func extractContextMatches(body string, terms []string, contextChars int) []messageMatch {
	snippets := extractContextChar(body, terms, contextChars)
	if len(snippets) == 0 {
		return nil
	}
	// Re-walk terms to attach char_offset/line for each merged snippet window.
	lowerBody := strings.ToLower(body)
	var matches []messageMatch
	seen := make(map[int]struct{})
	for _, term := range terms {
		if len(term) < 2 {
			continue
		}
		lowerTerm := strings.ToLower(term)
		termLen := len(term)
		searchFrom := 0
		for {
			idx := strings.Index(lowerBody[searchFrom:], lowerTerm)
			if idx < 0 {
				break
			}
			pos := searchFrom + idx
			searchFrom = pos + 1
			start, end := contextWindow(len(body), pos, termLen, contextChars)
			if _, ok := seen[start]; ok {
				continue
			}
			seen[start] = struct{}{}
			charOffset := pos
			line := lineNumberAt(body, pos)
			matches = append(matches, messageMatch{
				CharOffset: &charOffset,
				Snippet:    bodyByteSlice(body, start, end),
				Line:       &line,
			})
		}
	}
	if len(matches) == 0 {
		for _, snip := range snippets {
			charOffset, line := 0, 1
			matches = append(matches, messageMatch{CharOffset: &charOffset, Snippet: snip, Line: &line})
		}
	}
	return matches
}

// bodyContextSnippetsToMatches converts backend-provided body context snippets
// into messageMatch values. Body-search backends do not expose source offsets,
// so locations remain absent rather than triggering per-result body hydration.
func bodyContextSnippetsToMatches(snippets []string, truncated bool) ([]messageMatch, bool) {
	matches := make([]messageMatch, 0, len(snippets))
	for _, snippet := range snippets {
		if len(matches) >= maxContextSnippets {
			truncated = true
			break
		}
		matches = append(matches, messageMatch{
			Snippet: snippet,
		})
	}
	return matches, truncated
}

func floatArg(args map[string]any, key string, def float64) float64 {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	return v
}
