// Package chunkmatch converts stored vector chunk offsets into API-safe match
// excerpts. Stored offsets refer to preprocessed subject-plus-body text, so raw
// body locations are exposed only when the complete chunk is found exactly and
// unambiguously in the original body.
package chunkmatch

import (
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// Match is one semantically scored chunk excerpt. CharOffset and Line are nil
// unless the preprocessed chunk maps exactly and uniquely to the raw body.
type Match struct {
	CharOffset *int
	Snippet    string
	Line       *int
	Score      float64
}

// Build converts score-ordered chunk hits into bounded match excerpts.
// minScore filters excerpts only; it does not change message-level ranking.
func Build(
	subject, body string,
	cfg vector.Config,
	hits []vector.ChunkHit,
	minScore float64,
	maxMatches, snippetBytes int,
) ([]Match, bool) {
	if len(hits) == 0 || maxMatches <= 0 || snippetBytes <= 0 {
		return nil, false
	}

	preprocessed, _ := embed.Preprocess(subject, body, 0, preprocessConfig(cfg))
	prefixRunes := subjectPrefixRuneCount(subject)
	matches := make([]Match, 0, min(len(hits), maxMatches))
	qualifying := 0
	for _, hit := range hits {
		if hit.Score < minScore {
			continue
		}
		chunkText := runeSlice(preprocessed, hit.ChunkCharStart, hit.ChunkCharEnd)
		if chunkText == "" {
			continue
		}
		qualifying++
		if len(matches) >= maxMatches {
			continue
		}

		match := Match{
			Snippet: bytePrefix(chunkText, snippetBytes),
			Score:   hit.Score,
		}
		if hit.ChunkCharStart >= prefixRunes {
			if offset, ok := uniqueBodyOffset(body, chunkText); ok {
				line := strings.Count(body[:offset], "\n") + 1
				match.CharOffset = &offset
				match.Line = &line
			}
		}
		matches = append(matches, match)
	}
	return matches, qualifying > len(matches)
}

func preprocessConfig(cfg vector.Config) embed.PreprocessConfig {
	return embed.PreprocessConfig{
		StripQuotes:        cfg.Preprocess.StripQuotesEnabled(),
		StripSignatures:    cfg.Preprocess.StripSignaturesEnabled(),
		StripHTML:          cfg.Preprocess.StripHTMLEnabled(),
		StripBase64:        cfg.Preprocess.StripBase64Enabled(),
		StripURLTracking:   cfg.Preprocess.StripURLTrackingEnabled(),
		CollapseWhitespace: cfg.Preprocess.CollapseWhitespaceEnabled(),
	}
}

func subjectPrefixRuneCount(subject string) int {
	if subject == "" {
		return 0
	}
	return utf8.RuneCountInString("Subject: " + subject + "\n\n")
}

func runeSlice(s string, startRune, endRune int) string {
	if s == "" || startRune < 0 || endRune <= startRune {
		return ""
	}
	startByte := runeOffsetToByte(s, startRune)
	endByte := runeOffsetToByte(s, endRune)
	if startByte >= len(s) {
		return ""
	}
	return s[startByte:min(endByte, len(s))]
}

func runeOffsetToByte(s string, offset int) int {
	if offset <= 0 {
		return 0
	}
	walked := 0
	for i := range s {
		if walked >= offset {
			return i
		}
		walked++
	}
	return len(s)
}

func bytePrefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func uniqueBodyOffset(body, chunk string) (int, bool) {
	first := strings.Index(body, chunk)
	if first < 0 || strings.LastIndex(body, chunk) != first {
		return 0, false
	}
	return first, true
}
