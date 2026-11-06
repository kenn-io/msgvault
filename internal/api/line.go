package api

import "net/http"

// lineWebhookMiddleware dispatches the one signed ingress route before ordinary
// request security. The receiver takes the operation gate after authentication
// and validation. Neighboring routes keep their full existing protections,
// including encoded-path and method variations.
func (s *Server) lineWebhookMiddleware(next http.Handler) http.Handler {
	if s.lineWebhook == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.lineWebhook.Handles(r) {
			s.lineWebhook.Receive(w, r, s.beginMutation("LINE webhook receipt"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
