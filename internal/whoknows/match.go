package whoknows

import (
	"maps"
	"net/mail"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// automatedPatterns are sender local parts that belong to systems rather than
// people. A local part matches when it equals a pattern or starts with one
// followed by "-", "+", or ".". Underscores count as hyphens.
var automatedPatterns = []string{
	"no-reply", "noreply", "do-not-reply", "donotreply", "notification",
	"notifications", "notify", "alert", "alerts", "newsletter", "newsletters",
	"digest", "mailer-daemon", "postmaster", "bounce", "bounces", "automated",
	"robot", "bot", "calendar-notification",
}

// Automated reports whether an address looks like a system sender.
func Automated(email string) bool {
	if a, err := mail.ParseAddress(email); err == nil {
		email = a.Address
	}
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	local, _, _ = strings.Cut(local, "+")
	local = strings.ReplaceAll(local, "_", "-")
	for _, pattern := range automatedPatterns {
		if local == pattern {
			return true
		}
		if strings.HasPrefix(local, pattern) && len(local) > len(pattern) &&
			strings.ContainsRune("-.", rune(local[len(pattern)])) {
			return true
		}
	}
	return false
}

const ignoredWords = " a an the and or of in on for to with about vs versus "

// tokens splits text into case-folded words. A trailing run of "+" or "#"
// stays part of its word, so "C++" and "C#" differ from "C".
func tokens(s string) map[string]bool {
	runes := []rune(cases.Fold().String(norm.NFC.String(s)))
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }
	out := map[string]bool{}
	for i := 0; i < len(runes); {
		if !isWord(runes[i]) {
			i++
			continue
		}
		start := i
		for i < len(runes) && isWord(runes[i]) {
			i++
		}
		for i < len(runes) && (runes[i] == '+' || runes[i] == '#') {
			i++
		}
		if word := string(runes[start:i]); !strings.Contains(ignoredWords, " "+word+" ") {
			out[word] = true
		}
	}
	return out
}

// Tokens returns the case-folded words of s that Matches compares, sorted.
func Tokens(s string) []string {
	return slices.Sorted(maps.Keys(tokens(s)))
}

// Matches reports whether one side's words all appear on the other side.
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
