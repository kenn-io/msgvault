package personfacts

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// NormalizeDomain reduces a domain, URL, or email address to a lowercase
// ASCII host suitable for deterministic organization matching.
func NormalizeDomain(raw string) (string, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", errors.New("domain is required")
	}

	if !strings.Contains(candidate, "://") {
		if at := strings.LastIndex(candidate, "@"); at >= 0 {
			if at == 0 || at == len(candidate)-1 {
				return "", errors.New("email must contain a local part and host")
			}
			candidate = candidate[at+1:]
		}
	}

	host, err := extractDomainHost(candidate)
	if err != nil {
		return "", err
	}
	host = strings.TrimSuffix(host, ".")
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("convert domain to IDNA ASCII: %w", err)
	}
	ascii = strings.ToLower(strings.TrimSuffix(ascii, "."))
	if withoutWWW, ok := strings.CutPrefix(ascii, "www."); ok {
		ascii = withoutWWW
	}
	if err := validateDomainHost(ascii); err != nil {
		return "", err
	}
	return ascii, nil
}

func extractDomainHost(candidate string) (string, error) {
	if strings.Contains(candidate, "://") {
		parsed, err := url.Parse(candidate)
		if err != nil {
			return "", fmt.Errorf("parse domain URL: %w", err)
		}
		if parsed.Scheme == "" || parsed.Hostname() == "" {
			return "", errors.New("URL must contain a scheme and host")
		}
		return parsed.Hostname(), nil
	}
	if strings.ContainsAny(candidate, "/?#") {
		return "", errors.New("bare domain must not contain a path, query, or fragment")
	}
	parsed, err := url.Parse("//" + candidate)
	if err != nil {
		return "", fmt.Errorf("parse bare domain: %w", err)
	}
	if parsed.Hostname() == "" {
		return "", errors.New("domain host is required")
	}
	return parsed.Hostname(), nil
}

func validateDomainHost(host string) error {
	if host == "" || len(host) > 253 || !strings.Contains(host, ".") {
		return errors.New("domain must contain at least two labels")
	}
	if net.ParseIP(host) != nil {
		return errors.New("domain must not be an IP address")
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("domain contains an invalid label")
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < '0' || character > '9') && character != '-' {
				return errors.New("domain contains an invalid character")
			}
		}
	}
	return nil
}
