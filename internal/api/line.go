package api

import (
	"context"
	"net/http"
)

// lineWebhookMiddleware dispatches the one signed ingress route before ordinary
// request security. Receipts coordinate with backup freeze after authentication
// and validation. Neighboring routes keep their full existing protections,
// including encoded-path and method variations.
func (s *Server) lineWebhookMiddleware(next http.Handler) http.Handler {
	if s.lineWebhook == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.lineWebhook.Handles(r) {
			s.lineWebhook.Receive(w, r, func(context.Context) (func(), error) {
				s.backupFreeze.mu.Lock()
				if s.backupFreeze.active {
					s.backupFreeze.mu.Unlock()
					return nil, errMutationGateBusy
				}
				return s.backupFreeze.mu.Unlock, nil
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
