// Package sensitive detects likely credentials and financial identifiers in
// submitted text. It is advisory: no findings does not mean the text is safe.
package sensitive

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
)

// Finding describes a likely sensitive value. Start and End are byte offsets;
// Hint contains at most four characters of the value.
type Finding struct {
	Kind  string `json:"kind"`
	Start int    `json:"-"`
	End   int    `json:"-"`
	Hint  string `json:"hint,omitempty"`
}

var (
	cardPattern       = regexp.MustCompile(`\b[0-9](?:[ -]?[0-9]){12,}\b`)
	ibanStartPattern  = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}`)
	ibanPattern       = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]){11,30}\b`)
	ssnPattern        = regexp.MustCompile(`\b[0-9]{3}[- ][0-9]{2}[- ][0-9]{4}\b`)
	keyPattern        = regexp.MustCompile(`\b(?:[sr]k_live_[A-Za-z0-9]{24,}|sk-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,}|xox[bap]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16}|AIza[A-Za-z0-9_-]{35,}|eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})|-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----`)
	anchoredPattern   = regexp.MustCompile(`\b(?:([A-Za-z0-9_-]*(?i:password|passwd|pwd|passcode|secret|token|key)[A-Za-z0-9_-]*)["']?[ \t]*[:=][ \t]*|(?i:Bearer)[ \t]+)("(?:[^"\\\r\n]|\\[^\r\n])*"|'[^'\r\n]*'|[^"'\s]+)`)
	camelPattern      = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	acronymPattern    = regexp.MustCompile(`([A-Z])([A-Z][a-z])`)
	letterPattern     = regexp.MustCompile(`[A-Za-z]`)
	credentialPattern = regexp.MustCompile(`[0-9!@#$%^&*+/=~]`)
	cardValidator     = sync.OnceValue(func() *validator.Validate { return validator.New() })
)

// Scan returns findings in input order without changing the submitted text.
func Scan(text string) []Finding {
	var findings []Finding
	add := func(kind string, start, end int, hint string) {
		findings = append(findings, Finding{kind, start, end, hint})
	}
	for _, loc := range cardPattern.FindAllStringIndex(text, -1) {
		for start := loc[0]; start < loc[1]; {
			if end, value := cardCandidate(text, start, loc[1]); end > start {
				add("payment_card", start, end, "ending "+value[len(value)-4:])
				start = end
			}
			// Try each digit group, never the middle of an uninterrupted number.
			for start < loc[1] && text[start] >= '0' && text[start] <= '9' {
				start++
			}
			if start < loc[1] {
				start++
			}
		}
	}
	// Check each start independently so a candidate cannot hide a later IBAN.
	for _, loc := range ibanStartPattern.FindAllStringIndex(text, -1) {
		candidate := ibanPattern.FindStringIndex(text[loc[0]:])
		if candidate == nil {
			continue
		}
		loc[1] = loc[0] + candidate[1]
		length := ibanLengths[text[loc[0]:loc[0]+2]]
		end, size := loc[0], 0
		for end < loc[1] && size < length {
			if text[end] != ' ' {
				size++
			}
			end++
		}
		if length == 0 || size != length || end < loc[1] && text[end] != ' ' {
			continue
		}
		value := strings.ReplaceAll(text[loc[0]:end], " ", "")
		if validIBAN(value) {
			add("iban", loc[0], end, "ending "+value[len(value)-4:])
		}
	}
	for _, loc := range ssnPattern.FindAllStringIndex(text, -1) {
		value := text[loc[0]:loc[1]]
		if value[3] == value[6] && value[:3] != "000" && value[:3] != "666" && value[0] != '9' && value[4:6] != "00" && value[7:] != "0000" {
			add("us_ssn", loc[0], loc[1], "")
		}
	}
	for _, loc := range keyPattern.FindAllStringIndex(text, -1) {
		hint := ""
		if text[loc[0]] != '-' {
			hint = "starts " + text[loc[0]:loc[0]+4]
		}
		add("api_key", loc[0], loc[1], hint)
	}
	for _, loc := range anchoredPattern.FindAllStringSubmatchIndex(text, -1) {
		kind := "api_key"
		if loc[2] >= 0 {
			kind = anchorKind(text[loc[2]:loc[3]])
		}
		start, end := loc[4], loc[5]
		if text[start] == '"' || text[start] == '\'' {
			start++
			end--
		}
		value := text[start:end]
		if kind != "" && len(value) >= 8 && !strings.Contains(value, "://") &&
			(kind == "password" && strings.Contains(value, " ") || !strings.Contains(value, " ") && letterPattern.MatchString(value) && credentialPattern.MatchString(value)) {
			add(kind, start, end, "")
		}
	}
	slices.SortStableFunc(findings, func(a, b Finding) int { return a.Start - b.Start })
	kept, coveredEnd := findings[:0], 0
	for _, finding := range findings {
		if finding.Start >= coveredEnd {
			kept = append(kept, finding)
			coveredEnd = finding.End
		}
	}
	return kept
}

func anchorKind(identifier string) string {
	identifier = camelPattern.ReplaceAllString(acronymPattern.ReplaceAllString(identifier, "${1}_${2}"), "${1}_${2}")
	kind := ""
	for _, component := range strings.FieldsFunc(strings.ToLower(identifier), func(c rune) bool { return c == '_' || c == '-' }) {
		switch component {
		case "password", "passwd", "pwd", "passcode":
			return "password"
		case "secret", "token", "key":
			kind = "api_key"
		}
	}
	return kind
}

// Keep the longest valid card ending at a digit-group boundary.
func cardCandidate(text string, start, end int) (int, string) {
	var digits [19]byte
	size, bestEnd := 0, 0
	bestValue := ""
	for i := start; i < end && size < len(digits); i++ {
		if text[i] == ' ' || text[i] == '-' {
			continue
		}
		digits[size] = text[i]
		size++
		next := i + 1
		if (next == end || text[next] == ' ' || text[next] == '-') && size >= 13 && (digits[0] >= '2' && digits[0] <= '6' || digits[0] == '8') {
			value := string(digits[:size])
			if cardValidator().Var(value, "luhn_checksum") == nil {
				bestEnd, bestValue = next, value
			}
		}
	}
	return bestEnd, bestValue
}

// Country lengths from SWIFT IBAN Registry, release 101 (December 2025).
// https://www.swift.com/sites/default/files/files/iban-registry-v101.pdf
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16,
	"BG": 22, "BH": 22, "BI": 27, "BR": 29, "BY": 28, "CH": 21, "CR": 22,
	"CY": 28, "CZ": 24, "DE": 22, "DJ": 27, "DK": 18, "DO": 28, "EE": 20,
	"EG": 29, "ES": 24, "FI": 18, "FK": 18, "FO": 18, "FR": 27, "GB": 22,
	"GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28, "HN": 28, "HR": 21,
	"HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30,
	"KW": 30, "KZ": 20, "LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20,
	"LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22, "MK": 19, "MN": 20,
	"MR": 27, "MT": 31, "MU": 30, "NI": 28, "NL": 18, "NO": 15, "OM": 23,
	"PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22,
	"RU": 33, "SA": 24, "SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24,
	"SM": 27, "SO": 23, "ST": 25, "SV": 28, "TL": 23, "TN": 24, "TR": 26,
	"UA": 29, "VA": 22, "VG": 24, "XK": 20, "YE": 30,
}

func validIBAN(s string) bool {
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
