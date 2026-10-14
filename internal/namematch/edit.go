// Package namematch verifies already-normalized names with bounded rune edits.
package namematch

import "unicode"

// AtMostOne accepts equality or one insertion, deletion, substitution, or
// adjacent transposition. Normalization and minimum token length belong to
// the caller's matching policy.
func AtMostOne(left, right string) bool {
	return atMostOne(left, right, false)
}

// AtMostOneWithoutNumberEdit additionally rejects edits that touch a Unicode
// number. Unchanged numbers remain valid, including in mixed name tokens.
func AtMostOneWithoutNumberEdit(left, right string) bool {
	return atMostOne(left, right, true)
}

func atMostOne(left, right string, protectNumbers bool) bool {
	if left == right {
		return true
	}
	a, b := []rune(left), []rune(right)
	if len(a)-len(b) > 1 || len(b)-len(a) > 1 {
		return false
	}
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	if len(a) == len(b) {
		if i == len(a) {
			return true
		}
		if protectNumbers && (unicode.IsNumber(a[i]) || unicode.IsNumber(b[i])) {
			return false
		}
		if equalRunes(a[i+1:], b[i+1:]) {
			return true
		}
		return i+1 < len(a) && a[i] == b[i+1] && a[i+1] == b[i] &&
			(!protectNumbers || (!unicode.IsNumber(a[i+1]) && !unicode.IsNumber(b[i+1]))) &&
			equalRunes(a[i+2:], b[i+2:])
	}
	if len(a) < len(b) {
		a, b = b, a
	}
	return (!protectNumbers || !unicode.IsNumber(a[i])) && equalRunes(a[i+1:], b[i:])
}

func equalRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
