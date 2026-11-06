package youtube

import (
	"errors"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	cueTiming = regexp.MustCompile(`^(?:[0-9]{2,}:)?[0-5][0-9]:[0-5][0-9]\.[0-9]{3}[ \t]+-->[ \t]+(?:[0-9]{2,}:)?[0-5][0-9]:[0-5][0-9]\.[0-9]{3}(?:[ \t].*)?$`)
	cueMarkup = regexp.MustCompile(`<[^>]*>`)
)

// TranscriptText extracts cue text from an official WebVTT download. Timing,
// cue identifiers, notes, styles and regions are not transcript content.
func TranscriptText(vtt []byte) (string, error) {
	if !utf8.Valid(vtt) {
		return "", errors.New("invalid caption encoding")
	}
	text := strings.TrimPrefix(string(vtt), "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	if lines[0] != "WEBVTT" && !strings.HasPrefix(lines[0], "WEBVTT ") && !strings.HasPrefix(lines[0], "WEBVTT\t") {
		return "", errors.New("invalid caption header")
	}
	i := 1
	for i < len(lines) && lines[i] != "" {
		if strings.Contains(lines[i], "-->") {
			return "", errors.New("caption header lacks blank separator")
		}
		i++ // WebVTT header metadata ends at the first empty line.
	}
	var transcript []string
	for i < len(lines) {
		if lines[i] == "" {
			i++
			continue
		}
		start := i
		for i < len(lines) && lines[i] != "" {
			i++
		}
		block := lines[start:i]
		first := block[0]
		if first == "STYLE" || first == "REGION" || first == "NOTE" || strings.HasPrefix(first, "NOTE ") || strings.HasPrefix(first, "NOTE\t") {
			continue
		}
		timing := 0
		if !cueTiming.MatchString(first) {
			timing = 1 // A cue may start with one identifier line.
		}
		if timing >= len(block) || !cueTiming.MatchString(block[timing]) {
			return "", errors.New("invalid caption cue timing")
		}
		for _, line := range block[timing+1:] {
			// WebVTT payload cannot contain the raw timing delimiter. A second
			// cue without its blank separator must not become transcript text.
			if strings.Contains(line, "-->") {
				return "", errors.New("invalid caption cue boundary")
			}
			plain := strings.TrimSpace(html.UnescapeString(cueMarkup.ReplaceAllString(line, "")))
			if plain != "" {
				transcript = append(transcript, plain)
			}
		}
	}
	return strings.Join(transcript, "\n"), nil
}
