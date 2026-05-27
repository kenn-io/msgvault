package mbox

import (
	"fmt"
	"strings"
	"time"
)

// dateLayoutSpec holds precomputed metadata for a date layout string.
type dateLayoutSpec struct {
	layout    string
	numFields int
	hasTZ     bool
	tzIdx     int // index of MST token in fields, or -1
}

// fromSeparatorLayouts is the precomputed set of layout specs, built at init.
var fromSeparatorLayouts []dateLayoutSpec

func init() {
	for _, layout := range rawDateLayouts {
		fields := strings.Fields(layout)
		spec := dateLayoutSpec{
			layout:    layout,
			numFields: len(fields),
			hasTZ: strings.Contains(layout, "MST") ||
				strings.Contains(layout, "-0700") ||
				strings.Contains(layout, "-07:00"),
			tzIdx: -1,
		}
		for i, f := range fields {
			if f == "MST" {
				spec.tzIdx = i
				break
			}
		}
		fromSeparatorLayouts = append(fromSeparatorLayouts, spec)
	}
}

var rawDateLayouts = []string{
	"Mon Jan 2 15:04:05 2006",
	"Mon Jan 2 15:04:05 -0700 2006",
	"Mon Jan 2 15:04:05 -07:00 2006",
	"Mon Jan 2 15:04:05 MST 2006",
	"Mon Jan 2 15:04:05 2006 -0700",
	"Mon Jan 2 15:04:05 2006 -07:00",
	"Mon Jan 2 15:04:05 2006 MST",
	"Mon Jan 2 15:04 2006",
	"Mon Jan 2 15:04 -0700 2006",
	"Mon Jan 2 15:04 -07:00 2006",
	"Mon Jan 2 15:04 MST 2006",
	"Mon Jan 2 15:04 2006 -0700",
	"Mon Jan 2 15:04 2006 -07:00",
	"Mon Jan 2 15:04 2006 MST",
	"Jan 2 15:04:05 2006",
	"Jan 2 15:04:05 -0700 2006",
	"Jan 2 15:04:05 -07:00 2006",
	"Jan 2 15:04:05 MST 2006",
	"Jan 2 15:04:05 2006 -0700",
	"Jan 2 15:04:05 2006 -07:00",
	"Jan 2 15:04:05 2006 MST",
	"Jan 2 15:04 2006",
	"Jan 2 15:04 -0700 2006",
	"Jan 2 15:04 -07:00 2006",
	"Jan 2 15:04 MST 2006",
	"Jan 2 15:04 2006 -0700",
	"Jan 2 15:04 2006 -07:00",
	"Jan 2 15:04 2006 MST",
}

var fromSeparatorTZAbbrevOffsets = map[string]int{
	"UTC":  0,
	"GMT":  0,
	"UT":   0,
	"Z":    0,
	"EST":  -5 * 60 * 60,
	"EDT":  -4 * 60 * 60,
	"CST":  -6 * 60 * 60,
	"CDT":  -5 * 60 * 60,
	"MST":  -7 * 60 * 60,
	"MDT":  -6 * 60 * 60,
	"PST":  -8 * 60 * 60,
	"PDT":  -7 * 60 * 60,
	"AKST": -9 * 60 * 60,
	"AKDT": -8 * 60 * 60,
	"HST":  -10 * 60 * 60,
}

func offsetHHMM(offsetSeconds int) string {
	sign := '+'
	if offsetSeconds < 0 {
		sign = '-'
		offsetSeconds = -offsetSeconds
	}
	h := offsetSeconds / (60 * 60)
	m := (offsetSeconds % (60 * 60)) / 60
	return fmt.Sprintf("%c%02d%02d", sign, h, m)
}

func tzOffsetFromAbbrev(abbrev string) (int, bool) {
	abbrev = strings.Trim(abbrev, "()")
	abbrev = strings.ToUpper(abbrev)
	off, ok := fromSeparatorTZAbbrevOffsets[abbrev]
	return off, ok
}

func looksLikeTZToken(token string) bool {
	token = strings.Trim(token, "()")
	if _, ok := tzOffsetFromAbbrev(token); ok {
		return true
	}
	if looksLikeNumericOffset(token) {
		return true
	}
	if token == "" || len(token) > 5 {
		return false
	}
	if token != strings.ToUpper(token) {
		return false
	}
	for i := range len(token) {
		c := token[i]
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

func looksLikeNumericOffset(token string) bool {
	if len(token) < 5 {
		return false
	}
	if token[0] != '+' && token[0] != '-' {
		return false
	}
	// +HHMM format
	if len(token) == 5 {
		for i := 1; i < 5; i++ {
			if token[i] < '0' || token[i] > '9' {
				return false
			}
		}
		return true
	}
	// +HH:MM format
	if len(token) == 6 && token[3] == ':' {
		return token[1] >= '0' && token[1] <= '9' &&
			token[2] >= '0' && token[2] <= '9' &&
			token[4] >= '0' && token[4] <= '9' &&
			token[5] >= '0' && token[5] <= '9'
	}
	return false
}

// parseFromSeparatorDateFields is the shared parsing pipeline used by both the
// permissive and strict date parsers.
func parseFromSeparatorDateFields(fields []string, strict bool) (time.Time, bool) {
	if len(fields) < 6 || fields[0] != "From" {
		return time.Time{}, false
	}

	for _, spec := range fromSeparatorLayouts {
		if len(fields) < 2+spec.numFields {
			continue
		}

		if strict && !spec.hasTZ && len(fields) > 2+spec.numFields {
			if looksLikeTZToken(fields[2+spec.numFields]) {
				continue
			}
		}

		dateFields := fields[2 : 2+spec.numFields]
		dateStr := strings.Join(dateFields, " ")

		if spec.tzIdx == -1 {
			// No MST placeholder in layout
			if t, err := time.Parse(spec.layout, dateStr); err == nil {
				return t, true
			}
			continue
		}

		if !strict {
			// Permissive: try direct parse (Go maps unknown tz abbrevs to UTC)
			if t, err := time.Parse(spec.layout, dateStr); err == nil {
				return t, true
			}
			continue
		}

		// Strict: only accept known timezone abbreviations
		if spec.tzIdx >= len(dateFields) {
			continue
		}
		off, ok := tzOffsetFromAbbrev(dateFields[spec.tzIdx])
		if !ok {
			continue
		}

		patched := make([]string, len(dateFields))
		copy(patched, dateFields)
		patched[spec.tzIdx] = offsetHHMM(off)
		numericLayout := strings.Replace(spec.layout, "MST", "-0700", 1)
		if t, err := time.Parse(numericLayout, strings.Join(patched, " ")); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ParseFromSeparatorDate parses the ctime-like date portion of an mbox "From "
// separator line.
//
// This is intentionally permissive and is used as a heuristic for separator
// detection. In edge cases, an unescaped body line that looks like a separator
// ("From <x> <ctime-like date> ...") can be misclassified; mbox writers should
// escape such body lines (e.g. ">From ").
func ParseFromSeparatorDate(line string) (time.Time, bool) {
	return parseFromSeparatorDateFields(strings.Fields(line), false)
}

// ParseFromSeparatorDateStrict parses the ctime-like date portion of an mbox "From "
// separator line, but only accepts numeric offsets or a small allowlist of
// well-known timezone abbreviations. This avoids treating arbitrary abbreviations
// as UTC.
func ParseFromSeparatorDateStrict(line string) (time.Time, bool) {
	return parseFromSeparatorDateFields(strings.Fields(line), true)
}
