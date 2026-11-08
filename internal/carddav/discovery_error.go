package carddav

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var diagnosticHost = regexp.MustCompile(`^[a-zA-Z0-9.-]{1,253}$`)

// DiscoveryOriginError exposes only the refused origin, never a DAV href
// which can contain account identifiers or credentials.
type DiscoveryOriginError struct{ origin string }

func (e *DiscoveryOriginError) Error() string {
	if e.origin == "" {
		return "CardDAV discovery refused an unsafe href"
	}
	return "CardDAV discovery refused origin " + e.origin
}

func (e *DiscoveryOriginError) Unwrap() error { return ErrUnsafeHref }

func discoveryOriginError(target *url.URL) error {
	result := &DiscoveryOriginError{}
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") ||
		!diagnosticHost.MatchString(target.Hostname()) {
		return result
	}
	host := strings.ToLower(target.Hostname())
	if port := target.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return result
		}
		host += ":" + strconv.FormatUint(n, 10)
	}
	result.origin = target.Scheme + "://" + host
	return result
}
