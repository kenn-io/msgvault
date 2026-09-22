package carddav

import (
	"errors"
	"net/http"
	"strings"

	"github.com/icholy/digest"
)

var errUnsupportedDigestChallenge = errors.New("unsupported CardDAV Digest challenge")

// selectDigestChallenge never includes challenge bytes in its error. A server
// controls those bytes, and they can contain identifying or secret material.
func selectDigestChallenge(headers http.Header) (*digest.Challenge, error) {
	for _, value := range headers.Values("WWW-Authenticate") {
		if len(value) > 16<<10 {
			continue
		}
		for _, candidate := range splitAuthChallenges(value) {
			prefix, raw, ok := strings.Cut(strings.TrimSpace(candidate), " ")
			if !ok || !strings.EqualFold(prefix, "Digest") {
				continue
			}
			parts := splitUnquotedCommas(raw)
			seen := make(map[string]bool, len(parts))
			for index, part := range parts {
				key, parameter, hasValue := strings.Cut(strings.TrimSpace(part), "=")
				if !hasValue || !validDigestDirectiveKey(key) || seen[strings.ToLower(key)] {
					parts = nil
					break
				}
				key = strings.ToLower(key)
				seen[key] = true
				parts[index] = key + "=" + parameter
			}
			if parts == nil || !seen["realm"] || !seen["nonce"] {
				continue
			}
			challenge, err := digest.ParseChallenge(digest.Prefix + strings.Join(parts, ", "))
			if err != nil || challenge.Nonce == "" {
				continue
			}
			for index := range challenge.QOP {
				challenge.QOP[index] = strings.TrimSpace(challenge.QOP[index])
			}
			algorithm := strings.ToUpper(challenge.Algorithm)
			if algorithm != "" && algorithm != "MD5" && algorithm != "SHA-256" {
				continue
			}
			if len(challenge.QOP) > 0 && !challenge.SupportsQOP("auth") {
				continue
			}
			return challenge, nil
		}
	}
	return nil, errUnsupportedDigestChallenge
}

func validDigestDirectiveKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if r < 'A' || r > 'Z' {
			if r < 'a' || r > 'z' {
				if r < '0' || r > '9' {
					if r != '-' && r != '_' {
						return false
					}
				}
			}
		}
	}
	return true
}

func splitAuthChallenges(value string) []string {
	var challenges []string
	start := 0
	for _, comma := range unquotedCommaPositions(value) {
		rest := strings.TrimLeft(value[comma+1:], " \t")
		name, _, ok := strings.Cut(rest, " ")
		if !ok || name == "" || strings.ContainsAny(name, "=,\t") {
			continue
		}
		challenges = append(challenges, strings.TrimSpace(value[start:comma]))
		start = comma + 1
	}
	challenges = append(challenges, strings.TrimSpace(value[start:]))
	return challenges
}

func splitUnquotedCommas(value string) []string {
	positions := unquotedCommaPositions(value)
	parts := make([]string, 0, len(positions)+1)
	start := 0
	for _, comma := range positions {
		parts = append(parts, value[start:comma])
		start = comma + 1
	}
	return append(parts, value[start:])
}

func unquotedCommaPositions(value string) []int {
	positions := make([]int, 0, 4)
	quoted, escaped := false, false
	for index := 0; index < len(value); index++ {
		switch {
		case escaped:
			escaped = false
		case quoted && value[index] == '\\':
			escaped = true
		case value[index] == '"':
			quoted = !quoted
		case !quoted && value[index] == ',':
			positions = append(positions, index)
		}
	}
	return positions
}
