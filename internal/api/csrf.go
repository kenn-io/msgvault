package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const csrfHeaderName = "X-Csrf-Token"

const schemeHTTPS = "https"

type requestSecurityContextKey struct{}

type requestSecurity struct {
	auth   requestAuthentication
	scheme string
	host   string
}

func (s *Server) requestSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, host, err := s.effectiveRequestOrigin(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_forwarded_headers", "Invalid forwarded scheme or host")
			return
		}
		auth := s.classifyAPIRequestDirect(r)
		if auth.Mode == AuthModeLoopback && !s.keylessLoopbackHostAllowed(r) {
			writeError(w, http.StatusForbidden, "untrusted_host",
				"Keyless loopback daemon rejects requests for unrecognized hosts; connect via 127.0.0.1 or localhost")
			return
		}
		if auth.Mode == AuthModeSession && !ambientOriginAllowed(r, scheme, host) {
			writeError(w, http.StatusForbidden, "cross_origin_session",
				"Session-cookie requests must be same-origin; use API-key authentication for cross-origin access")
			return
		}
		if auth.Mode == AuthModeLoopback && !isSafeMethod(r.Method) && !ambientOriginAllowed(r, scheme, host) {
			writeError(w, http.StatusForbidden, "cross_origin_loopback",
				"Keyless loopback mutations must be same-origin; configure an API key for cross-origin access")
			return
		}
		security := requestSecurity{
			auth:   auth,
			scheme: scheme,
			host:   host,
		}
		ctx := context.WithValue(r.Context(), requestSecurityContextKey{}, security)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ambientOriginAllowed reports whether a request authorized by an ambient
// credential may proceed. Browser session cookies are ambient credentials, so
// they are strictly same-origin for every method (including GET/HEAD); keyless
// loopback access is equally ambient, so its unsafe methods get the same
// discipline: when the browser discloses an Origin it must exactly match the
// request's own scheme and host. Requests without an Origin header
// (same-origin navigations, curl, the TUI/CLI, other non-browser clients)
// pass. API-key requests never reach this check — the Authorization header is
// an explicit credential, not an ambient one — so cross-origin API clients
// are unaffected.
func ambientOriginAllowed(r *http.Request, scheme, host string) bool {
	if len(r.Header.Values("Origin")) == 0 {
		return true
	}
	return requestOriginMatches(r, scheme, host)
}

// keylessLoopbackHostAllowed defends a keyless (no API key) loopback-confined
// daemon against DNS rebinding. Such a daemon authorizes every request purely
// because it reached loopback, and the same-origin checks below derive their
// trusted host from r.Host — so an attacker page whose hostname is rebound to
// 127.0.0.1 would pass them. The request authority must instead match this
// server's real listener: a loopback literal or "localhost" on the actual
// bound port. Registered hostnames (the rebinding vector) are rejected.
//
// The guard is inert unless a real listener was bound (StartOnListener), so
// direct-handler unit tests are unaffected. It also does not apply when the
// daemon is bound non-loopback (the operator opted into unauthenticated
// remote access via AllowInsecure) or when the request arrives through an
// explicitly trusted proxy (a same-host reverse proxy may legitimately
// forward an external Host; rebinding attacks arrive as direct loopback
// connections, not through the operator's trusted proxy).
func (s *Server) keylessLoopbackHostAllowed(r *http.Request) bool {
	if !s.listenerBound || !s.cfg.Server.IsLoopback() || s.isTrustedProxy(r) {
		return true
	}
	return isLoopbackAuthority(r.Host, s.listenPort)
}

// isLoopbackAuthority reports whether host (an HTTP Host header, host or
// host:port) targets a loopback address or "localhost" on listenPort. A
// port present in host must equal listenPort; the hostname must be
// "localhost" (case-insensitive) or an IP literal that is loopback.
func isLoopbackAuthority(host string, listenPort int) bool {
	hostname := host
	if h, port, err := net.SplitHostPort(host); err == nil {
		hostname = h
		if listenPort != 0 && port != strconv.Itoa(listenPort) {
			return false
		}
	}
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func securityFromRequest(r *http.Request) (requestSecurity, bool) {
	security, ok := r.Context().Value(requestSecurityContextKey{}).(requestSecurity)
	return security, ok
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) || s.requestAuthentication(r).Mode != AuthModeSession {
			next.ServeHTTP(w, r)
			return
		}

		security, ok := securityFromRequest(r)
		if !ok || !requestOriginMatches(r, security.scheme, security.host) {
			writeError(w, http.StatusForbidden, "csrf_rejected", "Browser mutation origin or CSRF token is invalid")
			return
		}
		if !constantTimeTokenEqual(r.Header.Get(csrfHeaderName), security.auth.Session.CSRFToken) {
			writeError(w, http.StatusForbidden, "csrf_rejected", "Browser mutation origin or CSRF token is invalid")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func constantTimeTokenEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

func requestOriginMatches(r *http.Request, scheme, host string) bool {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || strings.Contains(origins[0], ",") {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.User != nil || origin.Scheme == "" || origin.Host == "" ||
		origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	return strings.EqualFold(origin.Scheme, scheme) && strings.EqualFold(origin.Host, host)
}

func (s *Server) effectiveRequestOrigin(r *http.Request) (string, string, error) {
	scheme := "http"
	if r.TLS != nil {
		scheme = schemeHTTPS
	}
	host := r.Host
	if !s.isTrustedProxy(r) {
		return scheme, host, nil
	}

	forwardedValues := r.Header.Values("Forwarded")
	xProtoValues := r.Header.Values("X-Forwarded-Proto")
	xHostValues := r.Header.Values("X-Forwarded-Host")
	if len(forwardedValues) > 0 && (len(xProtoValues) > 0 || len(xHostValues) > 0) {
		return "", "", errors.New("mixed Forwarded and X-Forwarded headers")
	}

	if len(forwardedValues) > 0 {
		forwardedScheme, forwardedHost, err := parseForwardedOrigin(forwardedValues)
		if err != nil {
			return "", "", err
		}
		if forwardedScheme != "" {
			scheme = forwardedScheme
		}
		if forwardedHost != "" {
			host = forwardedHost
		}
	} else {
		forwardedScheme, err := singleForwardedValue("X-Forwarded-Proto", xProtoValues)
		if err != nil {
			return "", "", err
		}
		forwardedHost, err := singleForwardedValue("X-Forwarded-Host", xHostValues)
		if err != nil {
			return "", "", err
		}
		if forwardedScheme != "" {
			scheme = strings.ToLower(forwardedScheme)
		}
		if forwardedHost != "" {
			host = forwardedHost
		}
	}

	if err := validateForwardedOrigin(scheme, host); err != nil {
		return "", "", err
	}
	return scheme, host, nil
}

func parseForwardedOrigin(values []string) (string, string, error) {
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", "", errors.New("ambiguous Forwarded header")
	}
	var scheme, host string
	for part := range strings.SplitSeq(values[0], ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return "", "", errors.New("malformed Forwarded parameter")
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return "", "", fmt.Errorf("malformed quoted Forwarded value: %w", err)
			}
			value = unquoted
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "proto":
			if value == "" {
				return "", "", errors.New("empty Forwarded proto value")
			}
			if scheme != "" {
				return "", "", errors.New("multiple Forwarded proto values")
			}
			scheme = strings.ToLower(value)
		case "host":
			if value == "" {
				return "", "", errors.New("empty Forwarded host value")
			}
			if host != "" {
				return "", "", errors.New("multiple Forwarded host values")
			}
			host = value
		}
	}
	return scheme, host, nil
}

func singleForwardedValue(name string, values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", fmt.Errorf("ambiguous %s header", name)
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return "", fmt.Errorf("empty %s header", name)
	}
	return value, nil
}

func validateForwardedOrigin(scheme, host string) error {
	if scheme != "http" && scheme != schemeHTTPS {
		return fmt.Errorf("unsupported forwarded scheme %q", scheme)
	}
	parsed, err := url.Parse(scheme + "://" + host)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Host != host ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid forwarded host %q", host)
	}
	return nil
}

func trustedProxyPrefixes(entries []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil {
			addr = addr.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return prefixes
}

func (s *Server) isTrustedProxy(r *http.Request) bool {
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range s.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
