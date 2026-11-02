package verification

import (
	"net/url"
	"slices"
	"strings"
)

// CredentialLinkSearchValues returns only payload-bearing URL components from
// a detected credential link. Hosts, endpoint names, and query parameter names
// are public context and must not hide a message from ordinary searches.
func CredentialLinkSearchValues(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
		return nil
	}

	var values []string
	seen := make(map[string]struct{})
	add := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	addVariants := func(value string, unescape func(string) (string, error)) {
		for range 4 {
			value = strings.TrimSpace(value)
			if value == "" {
				return
			}
			add(value)
			if credentialCodeComponent(value) {
				add(strings.NewReplacer(" ", "", "-", "").Replace(value))
			}
			decoded, err := unescape(value)
			if err != nil || decoded == value {
				return
			}
			value = decoded
		}
	}
	addPath := func(path string) {
		for component := range strings.SplitSeq(path, "/") {
			candidate := component
			for range 4 {
				decoded, err := url.PathUnescape(candidate)
				if err != nil {
					break
				}
				candidate = decoded
				// Public endpoint names are not credential payloads, even when
				// their length and characters match an opaque token.
				if credentialEndpoint(candidate) {
					break
				}
				if tokenPattern.MatchString(candidate) || credentialCodeComponent(candidate) {
					addVariants(candidate, url.PathUnescape)
					break
				}
				if !strings.Contains(candidate, "%") {
					break
				}
			}
		}
	}
	addQuery := func(query url.Values) {
		for key, candidates := range query {
			credentialKey := credentialEndpoint(key) || slices.Contains([]string{"code", "id", "ticket", "key", "nonce", "state"}, strings.ToLower(key))
			for _, candidate := range candidates {
				if credentialKey || tokenPattern.MatchString(candidate) {
					addVariants(candidate, url.QueryUnescape)
				}
			}
		}
	}

	// URL.Path is decoded by url.Parse. Split after decoding so an encoded
	// slash cannot hide a credential payload in the following path component.
	addPath(u.Path)
	addQuery(u.Query())

	// Fragments may contain query payloads (#token=...) or a client-side
	// route with a query (#/login?token=...).
	path, query, hasQuery := strings.Cut(u.Fragment, "?")
	if !hasQuery && strings.Contains(path, "=") {
		query, path = path, ""
	}
	fragmentQuery, _ := url.ParseQuery(query)
	addPath(path)
	addQuery(fragmentQuery)

	return values
}

func credentialCodeComponent(value string) bool {
	loc := codePattern.FindStringIndex(value)
	return loc != nil && loc[0] == 0 && loc[1] == len(value) && codeShape(value)
}
