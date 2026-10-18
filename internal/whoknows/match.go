package whoknows

import (
	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

func tokens(s string) map[string]bool {
	s = cases.Fold().String(norm.NFC.String(s))
	out := map[string]bool{}
	stop := " a an the and or of in on for to with about vs versus "
	runes := []rune(s)
	for i := 0; i < len(runes); {
		if !unicode.IsLetter(runes[i]) && !unicode.IsNumber(runes[i]) {
			i++
			continue
		}
		start := i
		for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsNumber(runes[i])) {
			i++
		}
		for i < len(runes) && (runes[i] == '+' || runes[i] == '#') {
			i++
		}
		word := string(runes[start:i])
		if !strings.Contains(stop, " "+word+" ") {
			out[word] = true
		}
	}
	return out
}

// Matches compares nonempty whole-token sets in either containment direction.
func Matches(topic, value string) bool {
	a, b := tokens(topic), tokens(value)
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	subset := func(x, y map[string]bool) bool {
		for k := range x {
			if !y[k] {
				return false
			}
		}
		return true
	}
	return subset(a, b) || subset(b, a)
}
func organizationMatches(topic string, o Organization) bool {
	candidates := tokens(o.NameNormalized)
	if len(candidates) == 0 {
		candidates = tokens(o.Name)
	}
	if domain, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(o.PrimaryDomain)); err == nil {
		candidates[strings.Split(domain, ".")[0]] = true
	}
	for token := range tokens(topic) {
		if utf8.RuneCountInString(token) >= 4 && candidates[token] {
			return true
		}
	}
	return false
}
func Automated(email string) bool {
	if a, err := mail.ParseAddress(email); err == nil {
		email = a.Address
	}
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	local, _, _ = strings.Cut(local, "+")
	for _, pattern := range AutomatedPatterns {
		if local == pattern {
			return true
		}
		if strings.HasPrefix(local, pattern) && len(local) > len(pattern) && strings.ContainsRune("-+._", rune(local[len(pattern)])) {
			return true
		}
	}
	return false
}
