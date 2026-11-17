package whoknows

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const ignoredWords = " a an the and or of in on for to with about vs versus who knows "

// tokens splits text into case-folded words. Combining marks stay inside
// their word, so Devanagari or Thai words that differ only by a vowel sign
// stay distinct. A trailing run of "+" or "#" stays part of its word, so
// "C++" and "C#" differ from "C".
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
		for i < len(runes) && (isWord(runes[i]) || unicode.IsMark(runes[i])) {
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

func subset(x, y map[string]bool) bool {
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

// Matches reports whether one side's words all appear on the other side.
func Matches(topic, value string) bool {
	a, b := tokens(topic), tokens(value)
	return len(a) > 0 && len(b) > 0 && (subset(a, b) || subset(b, a))
}

// Covers reports whether every word of topic appears in value.
func Covers(value, topic string) bool {
	t := tokens(topic)
	return len(t) > 0 && subset(t, tokens(value))
}
