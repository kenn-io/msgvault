// Package syncerr provides shared error classification for sync flows.
package syncerr

import (
	"errors"
	"net"
	"strings"
)

// IsTransientNetwork reports whether err (or anything it wraps) indicates a
// transient network problem rather than a logical or auth failure. It catches
// DNS lookup failures, dial timeouts, and connection refused — common after
// laptop sleep/wake or Wi-Fi flaps. Callers should retry on the next schedule
// instead of alerting the user; the underlying credentials are not invalid.
func IsTransientNetwork(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Some libraries (notably golang.org/x/oauth2) wrap transport errors
	// with fmt.Errorf without %w, hiding the typed *url.Error / *net.OpError
	// from errors.As. Fall back to substring matching for those cases.
	s := err.Error()
	switch {
	case strings.Contains(s, "dial tcp"):
		return true
	case strings.Contains(s, "no such host"):
		return true
	case strings.Contains(s, "i/o timeout"):
		return true
	case strings.Contains(s, "connection refused"):
		return true
	case strings.Contains(s, "network is unreachable"):
		return true
	case strings.Contains(s, "no route to host"):
		return true
	}
	return false
}
