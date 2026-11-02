package query

import (
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/verification"
)

// VerificationQueryTokens uses the existing query parser so free text and
// content filters receive the same guard. Structural account/message IDs are
// deliberately excluded. This reduces literal probing, not all search oracles.
func VerificationQueryTokens(q *search.Query) []string {
	if q == nil {
		return nil
	}
	var tokens []string
	for _, values := range [][]string{q.TextTerms, q.SubjectTerms, q.FromAddrs, q.ToAddrs, q.CcAddrs, q.BccAddrs, q.Labels, q.Filenames, q.ListIDs} {
		for _, value := range values {
			tokens = append(tokens, strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })...)
		}
	}
	for _, alternative := range q.Alternatives {
		tokens = append(tokens, VerificationQueryTokens(alternative)...)
	}
	return tokens
}

// VerificationQueryMatches examines the full canonical finding before any
// value is masked. Numeric fragments and opaque-token fragments are covered.
func VerificationQueryMatches(msg *MessageDetail, tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	finding := verification.Detect(VerificationInput(msg))
	if finding == nil {
		return false
	}
	for _, secret := range finding.Values {
		values := []string{secret.Value}
		if secret.Kind == "link" {
			values = verification.CredentialLinkSearchValues(secret.Value)
		}
		for _, raw := range values {
			value := strings.ToLower(raw)
			compact := strings.NewReplacer(" ", "", "-", "").Replace(value)
			for _, token := range tokens {
				if token != "" && (strings.Contains(value, token) || secret.Kind == "code" && strings.Contains(compact, token)) {
					return true
				}
			}
		}
	}
	return false
}
