// Package sensitive detects likely credentials and financial identifiers in
// submitted text. It is advisory: no findings does not mean the text is safe.
package sensitive

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Finding describes a likely sensitive value without retaining the value.
// Start and End are byte offsets in the input; Hint contains only a fixed
// label and, for cards/IBANs or recognized key prefixes, at most four characters.
// Scan treats all input as authored; callers may classify its origin separately.
type Finding struct {
	Kind       string
	Origin     string
	Start, End int
	Hint       string
}

var (
	cardPattern     = regexp.MustCompile(`[0-9](?:[ -]?[0-9]){12,}`)
	ibanPattern     = newIBANPattern()
	ssnPattern      = regexp.MustCompile(`[0-9]{3}[- ][0-9]{2}[- ][0-9]{4}`)
	keyPattern      = regexp.MustCompile(`sk_live_[A-Za-z0-9]{24,}|sk-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,}|xox[bap]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16}|AIza[A-Za-z0-9_-]{35,}|-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----|eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	tokenPattern    = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
	anchorPattern   = regexp.MustCompile(`(?i)\b(?:key|token|secret|bearer)\b`)
	passwordPattern = regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passcode)\b\s*(?::|=|\bis\b)\s*(\S{6,})`)
)

// Scan returns findings in input order. It performs no I/O and never changes
// the submitted text. Checksums and explicit anchors limit false positives.
func Scan(text string) []Finding {
	var findings []Finding
	add := func(kind string, start, end int, hint string) {
		findings = append(findings, Finding{Kind: kind, Origin: "authored", Start: start, End: end, Hint: hint})
	}
	for _, loc := range cardPattern.FindAllStringIndex(text, -1) {
		for start := loc[0]; start < loc[1]; {
			if end, value := cardCandidate(text, start, loc[1]); end > start {
				add("payment_card", start, end, "card ending "+value[len(value)-4:])
				start = end
			}
			// Only start another candidate after a separator, never inside an
			// uninterrupted oversized number. Accepted ranges do not overlap.
			for start < loc[1] && text[start] >= '0' && text[start] <= '9' {
				start++
			}
			if start < loc[1] {
				start++
			}
		}
	}
	for _, loc := range ibanPattern.FindAllStringIndex(text, -1) {
		value := compact(text[loc[0]:loc[1]])
		if bounded(text, loc[0], loc[1]) && validIBAN(value) {
			add("iban", loc[0], loc[1], "IBAN ending "+value[len(value)-4:])
		}
	}
	for _, loc := range ssnPattern.FindAllStringIndex(text, -1) {
		value := text[loc[0]:loc[1]]
		if bounded(text, loc[0], loc[1]) && value[3] == value[6] && value[:3] != "000" && value[:3] != "666" && value[0] != '9' && value[4:6] != "00" && value[7:] != "0000" {
			add("us_ssn", loc[0], loc[1], "US Social Security number")
		}
	}
	for _, loc := range keyPattern.FindAllStringIndex(text, -1) {
		if bounded(text, loc[0], loc[1]) {
			hint := "API key or private key"
			if text[loc[0]] != '-' {
				hint = "key prefix " + text[loc[0]:loc[0]+4]
			}
			add("api_key", loc[0], loc[1], hint)
		}
	}
	// Earlier detectors can cover a token, but tokens cannot cover one another.
	// Sweep just those earlier ranges once instead of searching all findings
	// for every token in a large draft.
	slices.SortStableFunc(findings, func(a, b Finding) int { return a.Start - b.Start })
	priorCount, priorIndex, coveredEnd := len(findings), 0, 0
	for _, loc := range tokenPattern.FindAllStringIndex(text, -1) {
		value := text[loc[0]:loc[1]]
		prefix := text[max(0, loc[0]-40):loc[0]]
		if !bounded(text, loc[0], loc[1]) || !hasTokenAnchor(prefix) || isHexOrUUID(value) || entropy(value) < 4 {
			continue
		}
		for priorIndex < priorCount && findings[priorIndex].Start <= loc[0] {
			coveredEnd = max(coveredEnd, findings[priorIndex].End)
			priorIndex++
		}
		if coveredEnd < loc[1] {
			add("api_key", loc[0], loc[1], "anchored secret token")
		}
	}
	for _, loc := range passwordPattern.FindAllStringSubmatchIndex(text, -1) {
		value := text[loc[2]:loc[3]]
		if !placeholder(value) {
			add("password", loc[0], loc[1], "password")
		}
	}
	slices.SortStableFunc(findings, func(a, b Finding) int { return a.Start - b.Start })
	return findings
}

func compact(s string) string { return strings.NewReplacer(" ", "", "-", "").Replace(s) }

func word(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_'
}

func bounded(text string, start, end int) bool {
	return (start == 0 || !word(text[start-1])) && (end == len(text) || !word(text[end]))
}

// Numeric runs may also contain another card, an expiry month, or a security
// code. Stop at the first valid separator boundary rather than absorbing them.
func cardCandidate(text string, start, end int) (int, string) {
	var digits [19]byte
	size := 0
	for i := start; i < end && size < len(digits); i++ {
		if text[i] == ' ' || text[i] == '-' {
			continue
		}
		digits[size] = text[i]
		size++
		next := i + 1
		if (next == end || text[next] == ' ' || text[next] == '-') && bounded(text, start, next) {
			value := string(digits[:size])
			if validCard(value) {
				return next, value
			}
		}
	}
	return 0, ""
}

func hasTokenAnchor(prefix string) bool {
	anchors := anchorPattern.FindAllStringIndex(prefix, -1)
	if len(anchors) == 0 {
		return false
	}
	context := prefix[anchors[len(anchors)-1][1]:]
	return len(context) <= 20 && !strings.ContainsAny(context, "\r\n/\\")
}

func validCard(s string) bool {
	if len(s) < 13 || len(s) > 19 {
		return false
	}
	issuer := s[0] == '4' || strings.HasPrefix(s, "34") || strings.HasPrefix(s, "37") || strings.HasPrefix(s, "6011") || strings.HasPrefix(s, "65") || s[:2] >= "51" && s[:2] <= "55" || s[:4] >= "2221" && s[:4] <= "2720"
	if !issuer || strings.Trim(s, s[:1]) == "" {
		return false
	}
	sum := 0
	for i := len(s) - 1; i >= 0; i-- {
		n := int(s[i] - '0')
		if (len(s)-1-i)%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// Common IBAN countries; unsupported countries are deliberately not guessed.
var ibanLengths = map[string]int{
	"AD": 24, "AT": 20, "BE": 16, "BG": 22, "CH": 21, "CY": 28, "CZ": 24,
	"DE": 22, "DK": 18, "EE": 20, "ES": 24, "FI": 18, "FR": 27, "GB": 22,
	"GI": 23, "GR": 27, "HR": 21, "HU": 28, "IE": 22, "IS": 26, "IT": 27,
	"LI": 21, "LT": 20, "LU": 20, "LV": 21, "MC": 27, "MT": 31, "NL": 18,
	"NO": 15, "PL": 28, "PT": 25, "RO": 24, "SE": 24, "SI": 19, "SK": 24,
	"AE": 23, "SA": 24, "TR": 26,
}

// Match each country's exact length, so following uppercase prose cannot be
// swallowed into a valid IBAN. Printed IBANs use groups of four characters.
func newIBANPattern() *regexp.Regexp {
	var patterns []string
	for country, length := range ibanLengths {
		grouped := `(?: [A-Z0-9]{4}){` + strconv.Itoa((length-4)/4) + `}`
		if remainder := length % 4; remainder != 0 {
			grouped += ` [A-Z0-9]{` + strconv.Itoa(remainder) + `}`
		}
		patterns = append(patterns, country+`[0-9]{2}(?:[A-Z0-9]{`+strconv.Itoa(length-4)+`}|`+grouped+`)`)
	}
	slices.Sort(patterns)
	return regexp.MustCompile(`\b(?:` + strings.Join(patterns, "|") + `)\b`)
}

func validIBAN(s string) bool {
	if len(s) < 4 || ibanLengths[s[:2]] != len(s) {
		return false
	}
	remainder := 0
	for _, c := range s[4:] + s[:4] {
		if c >= 'A' && c <= 'Z' {
			remainder = (remainder*100 + int(c-'A') + 10) % 97
		} else {
			remainder = (remainder*10 + int(c-'0')) % 97
		}
	}
	return remainder == 1
}

func isHexOrUUID(s string) bool {
	for _, c := range s {
		if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func entropy(s string) float64 {
	var counts [256]int
	for i := range len(s) {
		counts[s[i]]++
	}
	var result float64
	for _, count := range &counts {
		if count == 0 {
			continue
		}
		p := float64(count) / float64(len(s))
		result -= p * math.Log2(p)
	}
	return result
}

func placeholder(s string) bool {
	s = strings.ToLower(strings.Trim(s, "\"'.,;"))
	return strings.HasPrefix(s, "<") || strings.HasPrefix(s, "[") || strings.Trim(s, "*xX") == "" || s == "redacted" || s == "password" || s == "your" || s == "example" || s == "changeme"
}
