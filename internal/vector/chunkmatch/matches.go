// Package chunkmatch converts stored vector chunk offsets into API-safe match
// excerpts. Stored offsets refer to the preprocessed source identified by each
// hit's SourceBasis. Raw body locations are exposed only when the complete
// source span maps exactly to the original body.
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

	var ordinarySource string
	ordinarySourceReady := false
	ordinaryBodyStart := subjectPrefixRuneCount(subject)
	var bodySource string
	bodySourceReady := false
	matches := make([]Match, 0, min(len(hits), maxMatches))
	qualifying := 0
	for _, hit := range hits {
		if hit.Score < minScore {
			continue
		}
		var source string
		bodyStartRune := 0
		switch hit.SourceBasis {
		case vector.SourceBasisSubjectBody:
			if !ordinarySourceReady {
				ordinarySource, _ = embed.Preprocess(subject, body, 0, preprocessConfig(cfg))
				ordinarySourceReady = true
			}
			source = ordinarySource
			bodyStartRune = ordinaryBodyStart
		case vector.SourceBasisBody:
			if !bodySourceReady {
				bodySource, _ = embed.Preprocess("", body, 0, preprocessConfig(cfg))
				bodySourceReady = true
			}
			source = bodySource
		default:
			continue
		}
		chunkText, ok := runeSliceExact(source, hit.ChunkCharStart, hit.ChunkCharEnd)
		if !ok {
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
		if hit.ChunkCharStart >= bodyStartRune {
			var offset int
			var located bool
			switch hit.SourceBasis {
			case vector.SourceBasisSubjectBody:
				offset, located = uniqueBodyOffset(body, chunkText)
			case vector.SourceBasisBody:
				if bodySource == body {
					offset, located = positionalBodyOffset(body, hit.ChunkCharStart, chunkText)
				}
			}
			if located {
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

func runeSliceExact(s string, startRune, endRune int) (string, bool) {
	startByte, endByte, ok := runeByteRange(s, startRune, endRune)
	if !ok {
		return "", false
	}
	return s[startByte:endByte], true
}

func runeByteRange(s string, startRune, endRune int) (int, int, bool) {
	if s == "" || startRune < 0 || endRune <= startRune {
		return 0, 0, false
	}
	startByte := -1
	runeIndex := 0
	for byteOffset := range s {
		if runeIndex == startRune {
			startByte = byteOffset
		}
		if runeIndex == endRune {
			return startByte, byteOffset, startByte >= 0
		}
		runeIndex++
	}
	if runeIndex == endRune && startByte >= 0 {
		return startByte, len(s), true
	}
	return 0, 0, false
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

func positionalBodyOffset(body string, startRune int, chunk string) (int, bool) {
	endRune := startRune + utf8.RuneCountInString(chunk)
	startByte, endByte, ok := runeByteRange(body, startRune, endRune)
	if ok && body[startByte:endByte] == chunk {
		return startByte, true
	}
	return 0, false
}

func uniqueBodyOffset(body, chunk string) (int, bool) {
	first := strings.Index(body, chunk)
	if first < 0 || strings.LastIndex(body, chunk) != first {
		return 0, false
	}
	return first, true
}
