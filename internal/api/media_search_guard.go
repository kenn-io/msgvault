package api

import "net/http"

const (
	mediaSearchRequestsPerSecond = 1
	mediaSearchRequestBurst      = 2
)

func (s *Server) mediaSearchGuard(label string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.requestAuthentication(r).Mode == AuthModeLoopback &&
			s.crossOriginAmbientReadRequest(r) {
			writeError(w, http.StatusForbidden, "cross_origin_loopback",
				"Keyless loopback document requests must be same-origin; "+
					"configure an API key for cross-origin access")
			return
		}
		if !s.mediaSearchRateLimiter.Allow(clientIP(r)) {
			writeRateLimitExceeded(w)
			return
		}
		if s.operationGate != nil {
			done, ok := beginGateWorkBounded(r.Context(), s.operationGate, label)
			if !ok {
				writeOperationGateBusy(w, r, s.operationGate)
				return
			}
			defer done()
		}
		next(w, r)
	}
}
