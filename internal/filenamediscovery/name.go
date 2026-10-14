// Package filenamediscovery defines the bounded filename suggestion policy.
// It treats names as metadata, never as filesystem paths.
package filenamediscovery

import (
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/namematch"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	MaxQueryRunes  = 128
	MaxQueryTokens = 8
	MaxNameRunes   = 512
	MaxNameTokens  = 64
	MaxTokenRunes  = 64
)

type Query struct {
	Normalized string
	Tokens     []string
}

type Name struct {
	Normalized string
	Tokens     []string
}

// Rank orders clean matches ahead of typo matches.
type Rank uint8

const (
	FullExact Rank = iota
	TokensExact
	Prefix
	Typo
)

var (
	ErrInvalidQuery = errors.New("filename query requires 1–8 tokens within 128 runes and 64 runes per token")
	caseFolder      = cases.Fold() // Fold is stateless and safe for concurrent use.
)

// ParseQuery retains token order and repetitions, counting limits before any
// candidate retrieval. It rejects rather than truncates an oversized query.
func ParseQuery(raw string) (Query, error) {
	normalized := normalize(raw)
	if utf8.RuneCountInString(normalized) > MaxQueryRunes {
		return Query{}, ErrInvalidQuery
	}
	tokens, ok := tokenize(normalized, MaxQueryTokens)
	if !ok || len(tokens) == 0 {
		return Query{}, ErrInvalidQuery
	}
	return Query{Normalized: normalized, Tokens: tokens}, nil
}

// ProjectName excludes an oversized name as a whole. Within-limit names with
// no tokens remain valid projection entries but cannot match a valid query.
func ProjectName(raw string) (Name, bool) {
	normalized := normalize(raw)
	if utf8.RuneCountInString(normalized) > MaxNameRunes {
		return Name{}, false
	}
	tokens, ok := tokenize(normalized, MaxNameTokens)
	if !ok {
		return Name{}, false
	}
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, exists := seen[token]; !exists {
			seen[token] = struct{}{}
			unique = append(unique, token)
		}
	}
	return Name{Normalized: normalized, Tokens: unique}, true
}

func normalize(raw string) string {
	return norm.NFC.String(caseFolder.String(norm.NFC.String(raw)))
}

func tokenize(normalized string, maxTokens int) ([]string, bool) {
	var tokens []string
	var token []rune
	flush := func() {
		if len(token) != 0 {
			tokens = append(tokens, string(token))
			token = token[:0]
		}
	}
	for _, r := range normalized {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || (unicode.IsMark(r) && len(token) != 0) {
			token = append(token, r)
			if len(token) > MaxTokenRunes {
				return nil, false
			}
		} else {
			flush()
			if len(tokens) > maxTokens {
				return nil, false
			}
		}
	}
	flush()
	return tokens, len(tokens) <= maxTokens
}

// Match requires every query token and permits at most one typo-needing term.
// Repeated terms may reuse a candidate token, as in Directory conjunctions.
func Match(q Query, name Name) (Rank, bool) {
	if len(q.Tokens) == 0 || len(name.Tokens) == 0 {
		return 0, false
	}
	if q.Normalized == name.Normalized {
		return FullExact, true
	}
	rank, typoTerms := TokensExact, 0
	for _, term := range q.Tokens {
		best := Typo + 1
		for _, token := range name.Tokens {
			if term == token {
				best = TokensExact
				break
			}
			if allNumbers(term) || allNumbers(token) {
				continue
			}
			if strings.HasPrefix(token, term) {
				best = min(best, Prefix)
				continue
			}
			if utf8.RuneCountInString(term) >= 5 && namematch.AtMostOneWithoutNumberEdit(term, token) {
				best = min(best, Typo)
			}
		}
		if best > Typo {
			return 0, false
		}
		if best == Typo {
			typoTerms++
			if typoTerms > 1 {
				return 0, false
			}
		}
		rank = max(rank, best)
	}
	return rank, true
}

func allNumbers(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if !unicode.IsNumber(r) {
			return false
		}
	}
	return true
}

// DeleteKeys returns unique non-number rune-deletion keys in deterministic
// string order. Number edits cannot match, so they need no postings.
// Retrieval must still verify the actual token: key collisions are expected.
func DeleteKeys(token string) []string {
	runes := []rune(token)
	seen := make(map[string]struct{}, len(runes))
	for i, r := range runes {
		if unicode.IsNumber(r) {
			continue
		}
		seen[string(runes[:i])+string(runes[i+1:])] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
