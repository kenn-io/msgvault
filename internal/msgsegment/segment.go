// Package msgsegment labels structural quote and forward markers without changing
// archived content. Authored means no relay marker was recognized, not proven authorship.
package msgsegment

import (
	"strings"
	"unicode/utf8"
)

const Version = "seg-v1"
const MaxBodyBytes = 1 << 20

type Kind string

const (
	Authored  Kind = "authored"
	Quoted    Kind = "quoted"
	Forwarded Kind = "forwarded"
	Signature Kind = "signature"
)

type Format string

const (
	Text Format = "text"
	HTML Format = "html"
	Chat Format = "chat"
)

// Claimed contains spoofable forwarded headers. Never use these as identity or recipient data.
type Claimed struct {
	From         string `json:"from,omitempty"`
	Date         string `json:"date,omitempty"`
	Subject      string `json:"subject,omitempty"`
	To           string `json:"to,omitempty"`
	ContentTrust string `json:"content_trust"`
}
type Segment struct {
	Kind       Kind     `json:"kind"`
	Start      int      `json:"start"`
	End        int      `json:"end"`
	Basis      string   `json:"basis"`
	Confidence string   `json:"confidence"`
	Depth      int      `json:"depth,omitempty"`
	Trust      string   `json:"trust"`
	Claimed    *Claimed `json:"claimed,omitempty"`
}
type Options struct{ Subject string }
type Result struct {
	Version   string    `json:"segments_version"`
	Segments  []Segment `json:"segments"`
	Truncated bool      `json:"segments_truncated,omitempty"`
	body      string
}

// AuthoredText joins only residual authored ranges, excluding signatures.
func (r Result) AuthoredText() string {
	var b strings.Builder
	for _, s := range r.Segments {
		if s.Kind == Authored {
			b.WriteString(r.body[s.Start:s.End])
		}
	}
	return b.String()
}

// KindsOverlapping returns the kinds touched by a nonempty byte span.
func (r Result) KindsOverlapping(start, end int) []Kind {
	if start < 0 || end > len(r.body) || start >= end {
		return nil
	}
	var kinds []Kind
	for _, s := range r.Segments {
		if s.Start < end && s.End > start {
			kinds = append(kinds, s.Kind)
		}
	}
	return kinds
}
func segment(kind Kind, start, end int, basis string, depth int) Segment {
	trust := "sender_words"
	if kind == Quoted || kind == Forwarded {
		trust = "relayed"
	}
	if kind == Signature {
		trust = "boilerplate"
	}
	confidence := "marker"
	if kind == Authored {
		confidence = "heuristic"
	}
	return Segment{Kind: kind, Start: start, End: end, Basis: basis, Depth: depth, Trust: trust, Confidence: confidence}
}
func addSegment(r *Result, s Segment) {
	if s.Start == s.End {
		return
	}
	if n := len(r.Segments); n > 0 {
		last := &r.Segments[n-1]
		if last.End == s.Start && last.Kind == s.Kind && last.Basis == s.Basis && last.Depth == s.Depth && last.Claimed == nil && s.Claimed == nil && last.Confidence == s.Confidence {
			last.End = s.End
			return
		}
	}
	r.Segments = append(r.Segments, s)
}

// Split returns ordered byte ranges that tile the exact input, including line
// endings and markup. Processing is capped; the unexamined tail stays authored.
func Split(body string, format Format, opts Options) Result {
	r := Result{Version: Version, Segments: []Segment{}, body: body}
	if body == "" {
		return r
	}
	if format == Chat {
		addSegment(&r, segment(Authored, 0, len(body), "unmarked", 0))
		return r
	}
	limit := min(len(body), MaxBodyBytes)
	for limit < len(body) && limit > 0 && !utf8.RuneStart(body[limit]) {
		limit--
	}
	r.Truncated = limit < len(body)
	if format == Text {
		splitText(&r, body[:limit], opts)
	} else {
		addSegment(&r, segment(Authored, 0, limit, "unmarked", 0))
	}
	if r.Truncated {
		addSegment(&r, segment(Authored, limit, len(body), "unexamined_tail", 0))
	}
	return r
}

type line struct {
	start, end int
	text       string
}

func linesOf(body string) []line {
	var lines []line
	for start := 0; start < len(body); {
		end := len(body)
		if i := strings.IndexByte(body[start:], '\n'); i >= 0 {
			end = start + i + 1
		}
		lines = append(lines, line{start, end, strings.TrimSuffix(strings.TrimSuffix(body[start:end], "\n"), "\r")})
		start = end
	}
	return lines
}
func quoteDepth(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	for strings.HasPrefix(s, ">") {
		n++
		s = strings.TrimSpace(s[1:])
	}
	return n
}
func fence(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return ""
	}
	n := 1
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n >= 3 {
		if s[0] == '`' && strings.ContainsRune(s[n:], '`') {
			return ""
		}
		return s[:n]
	}
	return ""
}
func splitText(r *Result, body string, opts Options) {
	lines := linesOf(body)
	code := ""
	kind, basis := Authored, "unmarked"
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l.text)
		if code != "" {
			if f := fence(t); strings.HasPrefix(f, code) && strings.TrimSpace(t[len(f):]) == "" {
				code = ""
			}
			addSegment(r, segment(kind, l.start, l.end, basis, 0))
			continue
		}
		if f := fence(t); f != "" {
			code = f
			addSegment(r, segment(kind, l.start, l.end, basis, 0))
			continue
		}
		if forwardMarker(t) || (originalMarker(t) && forwardSubject(opts.Subject)) {
			s := segment(Forwarded, l.start, len(body), "fwd_marker", 1)
			s.Claimed = claimedHeaders(lines, i+1)
			addSegment(r, s)
			return
		}
		if originalMarker(t) || headerBlock(lines, i) {
			start := i
			if originalMarker(t) {
				start = i + 1
				for start < len(lines) && strings.TrimSpace(lines[start].text) == "" {
					start++
				}
			}
			if headerBlock(lines, start) {
				addSegment(r, segment(Quoted, l.start, len(body), "header_block", 1))
				return
			}
		}
		if l.text == "-- " || t == "Sent from my iPhone" || t == "Get Outlook for iOS" {
			kind, basis = Signature, "signature_marker"
			addSegment(r, segment(kind, l.start, l.end, basis, 0))
			continue
		}
		// Attribution is recognized only immediately before an angle-prefixed quote.
		found := false
		for count := 1; count <= 2 && i+count <= len(lines); count++ {
			attribution := t
			if count == 2 {
				if !wrappedAttributionStart(t) || attributionLine(lines[i+1].text) {
					continue
				}
				attribution += " " + strings.TrimSpace(lines[i+1].text)
			}
			if !attributionLine(attribution) {
				continue
			}
			next := i + count
			for next < len(lines) && strings.TrimSpace(lines[next].text) == "" {
				next++
			}
			if next < len(lines) && quoteDepth(lines[next].text) > 0 {
				kind, basis = Authored, "unmarked"
				addSegment(r, segment(Quoted, l.start, lines[next].start, "attribution_line", 1))
				i = next - 1
				found = true
				break
			}
		}
		if found {
			continue
		}
		if depth := quoteDepth(t); depth > 0 {
			kind, basis = Authored, "unmarked"
			addSegment(r, segment(Quoted, l.start, l.end, "angle_prefix", depth))
			continue
		}
		addSegment(r, segment(kind, l.start, l.end, basis, 0))
	}
}
