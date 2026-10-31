package api

import "net/http"

// lineWebhookMiddleware dispatches the one signed ingress route before ordinary
// request security and the operation gate. Neighboring routes keep their full
// existing protections, including encoded-path and method variations.
func (s *Server) lineWebhookMiddleware(next http.Handler) http.Handler {
	if s.lineWebhook == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.lineWebhook.Handles(r) {
			s.lineWebhook.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
