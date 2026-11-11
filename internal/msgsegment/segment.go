// Package msgsegment labels byte ranges in plain-text message bodies.
package msgsegment

import (
	"regexp"
	"strings"
)

const (
	Version      = "seg-v2"
	MaxBodyBytes = 1 << 20
)

type Kind string

const (
	Authored  Kind = "authored"
	Quoted    Kind = "quoted"
	Forwarded Kind = "forwarded"
	Signature Kind = "signature"
)

type Segment struct {
	Kind  Kind `json:"kind"`
	Start int  `json:"start"`
	End   int  `json:"end"`
	Depth int  `json:"depth,omitzero" jsonschema:"Quote nesting depth; present only on quoted ranges. Attribution lines and unprefixed history blocks have depth 1."`
}

type Result struct {
	Segments  []Segment
	Truncated bool
}

// datedAttribution matches a reply header start: "On", then a date with a year, clock time or numeric day.
var datedAttribution = regexp.MustCompile(`(?i)^On\s+(?:(?:Mon(?:day)?|Tue(?:sday)?|Wed(?:nesday)?|Thu(?:rsday)?|Fri(?:day)?|Sat(?:urday)?|Sun(?:day)?|Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:t(?:ember)?)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\b|\d).*?(?:\b(?:19|20)\d{2}\b|\d{1,2}:\d{2}|\d{1,2}/\d{1,2})`)
var wroteSuffix = regexp.MustCompile(`(?i)\bwrote:\s*$`)

// Thunderbird's dated reply forms, used only by Split.
// https://github.com/thunderbird/thunderbird-l10n/blob/main/de/mail/chrome/messenger/messengercompose/composeMsgs.properties#L174-L178
// https://github.com/thunderbird/thunderbird-l10n/blob/main/fr/mail/chrome/messenger/messengercompose/composeMsgs.properties#L174-L178
// https://github.com/thunderbird/thunderbird-l10n/blob/86b8c23e4e72ad2f8c7663afed8e0ac308f9213c/es-ES/mail/chrome/messenger/messengercompose/composeMsgs.properties#L174-L184
// https://github.com/thunderbird/thunderbird-l10n/blob/86b8c23e4e72ad2f8c7663afed8e0ac308f9213c/it/mail/chrome/messenger/messengercompose/composeMsgs.properties#L174-L184
// https://github.com/thunderbird/thunderbird-l10n/blob/86b8c23e4e72ad2f8c7663afed8e0ac308f9213c/pt-BR/mail/chrome/messenger/messengercompose/composeMsgs.properties#L174-L184
// https://github.com/thunderbird/thunderbird-l10n/blob/86b8c23e4e72ad2f8c7663afed8e0ac308f9213c/pt-PT/mail/chrome/messenger/messengercompose/composeMsgs.properties#L174-L184
const (
	thunderbirdDate = `.*\p{Nd}.*`
	thunderbirdTime = `\d{1,2}:\d{2}(?:[\s\p{Z}]*(?:AM|PM|[AP]\.[\s\p{Z}]*M\.))?`
)

var thunderbirdDatedAttribution = regexp.MustCompile(`(?i)^(?:` +
	`Am\s+.+\s+um\s+\d{1,2}:\d{2}\s+schrieb\s+.+:` +
	`|.+\s+schrieb\s+am\s+.+\s+um\s+\d{1,2}:\d{2}:` +
	`|Le\s+.+\s+à\s+\d{1,2}:\d{2},\s+.+\s+a\s+écrit[\s\x{A0}]+:` +
	`|.+\s+a\s+écrit\s+le\s+.+\s+à\s+\d{1,2}:\d{2}[\s\x{A0}]+:` +
	`|El\s+` + thunderbirdDate + `\s+a\s+las\s+` + thunderbirdTime + `,\s+.+\s+escribió:` +
	`|.+\s+escribió\s+el\s+` + thunderbirdDate + `\s+a\s+las\s+` + thunderbirdTime + `:` +
	`|Il\s+` + thunderbirdDate + `\s+` + thunderbirdTime + `,\s+.+\s+ha\s+scritto:` +
	`|.+\s+ha\s+scritto\s+il\s+` + thunderbirdDate + `\s+alle\s+` + thunderbirdTime + `:` +
	`|Em\s+` + thunderbirdDate + `\s+` + thunderbirdTime + `,\s+.+\s+escreveu:` +
	`|.+\s+escreveu\s+em\s+` + thunderbirdDate + `\s+` + thunderbirdTime + `:` +
	`|Às\s+` + thunderbirdTime + `\s+de\s+` + thunderbirdDate + `,\s+.+\s+escreveu:` +
	`|.+\s+escreveu\s+às\s+` + thunderbirdTime + `\s+de\s+` + thunderbirdDate + `:` +
	`)\s*$`)

var relaySeparator = regexp.MustCompile(`(?i)^-{2,}\s*(?:(original message|original-nachricht|message original|mensaje original|messaggio originale|mensagem original)|forwarded message|weitergeleitete nachricht|message transféré|mensaje reenviado|messaggio inoltrato|mensagem encaminhada|mensagem reencaminhada)\s*-{2,}$`)
var outlookDivider = regexp.MustCompile(`^_{10,}$`)

// outlookHistory requires consecutive From and Sent lines and a Subject
// within the first 64 lines, before any blank line. Date can replace Sent
// when an underscore divider precedes the block.
func outlookHistory(lines []string) bool {
	divider := outlookDivider.MatchString(strings.TrimSpace(lines[0]))
	if divider {
		lines = lines[1:]
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}
	if len(lines) < 3 || !headerValue(lines[0], "from:") ||
		!headerValue(lines[1], "sent:") && (!divider || !headerValue(lines[1], "date:")) {
		return false
	}
	for _, line := range lines[2:min(64, len(lines))] {
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			return false
		}
		if strings.HasPrefix(line, "subject:") {
			return true
		}
	}
	return false
}

func headerValue(line, key string) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(line, key) && strings.TrimSpace(line[len(key):]) != ""
}

// IsDatedAttribution reports whether a line is a complete dated reply header.
func IsDatedAttribution(line string) bool {
	return datedAttribution.MatchString(line) && wroteSuffix.MatchString(line)
}

// QuotedHistoryFollows reports whether the next nonblank line is angle-quoted.
func QuotedHistoryFollows(lines []string) bool {
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return strings.HasPrefix(trimmed, ">")
		}
	}
	return false
}

// Split tiles body with absolute, half-open byte ranges. Authored means that
// no relay marker was recognized, not that authorship has been verified.
// It examines at most MaxBodyBytes, ending at a complete line. Unexamined
// bytes retain the last label, and Truncated reports that limit.
func Split(body string) Result {
	scanEnd := len(body)
	truncated := scanEnd > MaxBodyBytes
	if truncated {
		scanEnd = strings.LastIndexByte(body[:MaxBodyBytes], '\n') + 1
	}
	lines := strings.SplitAfter(body[:scanEnd], "\n")
	result := Result{Truncated: truncated}
	kind, depth := Authored, 0
	relay := false
	start, blankStart := 0, -1
	appendRange := func(kind Kind, depth, start, end int) {
		if n := len(result.Segments); n > 0 && result.Segments[n-1].Kind == kind && result.Segments[n-1].Depth == depth {
			result.Segments[n-1].End = end
		} else {
			result.Segments = append(result.Segments, Segment{Kind: kind, Start: start, End: end, Depth: depth})
		}
	}
	for i, raw := range lines {
		if raw == "" {
			continue
		}
		line := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
		trimmed := strings.TrimSpace(line)
		end := start + len(raw)
		if trimmed == "" {
			if blankStart < 0 {
				blankStart = start
			}
			start = end
			continue
		}
		if !relay {
			depth = 0
			m := relaySeparator.FindStringSubmatch(trimmed)
			switch {
			case m != nil:
				kind, depth = Forwarded, 0
				if m[1] != "" {
					kind, depth = Quoted, 1
				}
				relay = true
			case strings.EqualFold(trimmed, "Begin forwarded message:"):
				kind, relay = Forwarded, true
			case outlookHistory(lines[i:]):
				kind, depth, relay = Quoted, 1, true
			case strings.HasPrefix(trimmed, ">"):
				kind = Quoted
			case (IsDatedAttribution(trimmed) || thunderbirdDatedAttribution.MatchString(trimmed)) && QuotedHistoryFollows(lines[i+1:]):
				kind, depth = Quoted, 1
			case line == "-- ":
				kind = Signature
			case kind == Signature:
			default:
				kind = Authored
			}
		}
		if kind == Quoted {
			depth = 0
			for strings.HasPrefix(trimmed, ">") {
				depth++
				trimmed = strings.TrimSpace(trimmed[1:])
			}
			depth = max(1, depth)
		}
		if blankStart >= 0 {
			blankKind, blankDepth := Authored, 0
			if n := len(result.Segments); n > 0 && kind != Authored && result.Segments[n-1].Kind != Authored {
				blankKind, blankDepth = result.Segments[n-1].Kind, result.Segments[n-1].Depth
			}
			appendRange(blankKind, blankDepth, blankStart, start)
			blankStart = -1
		}
		appendRange(kind, depth, start, end)
		start = end
	}
	if blankStart >= 0 {
		appendRange(kind, depth, blankStart, start)
	}
	if start < len(body) {
		if len(result.Segments) == 0 {
			result.Segments = []Segment{{Kind: kind, Start: 0, End: len(body), Depth: depth}}
		} else {
			result.Segments[len(result.Segments)-1].End = len(body)
		}
	}
	return result
}
