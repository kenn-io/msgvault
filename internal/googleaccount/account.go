// Package googleaccount compares Google account addresses without depending on
// credential management or provider clients.
package googleaccount

import "strings"

// Same reports whether two addresses identify the same Google account. Gmail
// ignores dots and plus suffixes and treats googlemail.com as gmail.com.
// Workspace aliases require an exact, case-insensitive match.
func Same(expected, canonical string) bool {
	if strings.EqualFold(expected, canonical) {
		return true
	}
	expectedNorm := normalizeGmailAddress(expected)
	canonicalNorm := normalizeGmailAddress(canonical)
	return expectedNorm != "" && expectedNorm == canonicalNorm
}

func normalizeGmailAddress(email string) string {
	local, domain, ok := strings.CutLast(email, "@")
	if !ok {
		return ""
	}
	local = strings.ToLower(local)
	domain = strings.ToLower(domain)
	if domain != "gmail.com" && domain != "googlemail.com" {
		return ""
	}
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	local = strings.ReplaceAll(local, ".", "")
	return local + "@gmail.com"
}
