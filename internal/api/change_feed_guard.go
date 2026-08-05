package api

import (
	"net/http"
	"strings"
)

const (
	changeFeedRequestsPerSecond = 2
	changeFeedRequestBurst      = 4
)

// changeFeedGuard protects the one GET endpoint whose storage path briefly
// takes SQLite's writer lock. It runs inside requestSecurityMiddleware, so the
// request already carries the effective origin and authentication mode.
func (s *Server) changeFeedGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.requestAuthentication(r).Mode == AuthModeLoopback &&
			s.crossOriginChangeFeedRequest(r) {
			writeError(w, http.StatusForbidden, "cross_origin_loopback",
				"Keyless loopback change-feed requests must be same-origin; "+
					"configure an API key for cross-origin access")
			return
		}

		// This limiter has no trusted-loopback exemption. Even an authenticated
		// local caller can otherwise acquire the database lock in a tight loop.
		if !s.changesRateLimiter.Allow(clientIP(r)) {
			writeRateLimitExceeded(w)
			return
		}
		next(w, r)
	}
}

func (s *Server) crossOriginChangeFeedRequest(r *http.Request) bool {
	if len(r.Header.Values("Origin")) > 0 {
		security, ok := securityFromRequest(r)
		if !ok || !ambientOriginAllowed(r, security.scheme, security.host) {
			return true
		}
	}

	values := r.Header.Values("Sec-Fetch-Site")
	if len(values) == 0 {
		return false
	}
	if len(values) != 1 {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(values[0])) {
	case "same-origin", "none":
		return false
	default:
		return true
	}
}
