package api

import (
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/query"
)

// contentReadMiddleware retains the cleanup read lease until the response has
// finished, including cached/inline bytes loaded before a destructive mutation.
func (s *Server) contentReadMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read := r.Method == http.MethodGet || r.Method == http.MethodHead || readOnlyPostRouteRequest(r)
		if !read || s.contentReadGuard == nil || r.URL.Path == "/health" || r.URL.Path == "/api/v1/health" || r.URL.Path == "/api/ping" || r.URL.Path == "/api/v1/sources/status" {
			next.ServeHTTP(w, r)
			return
		}
		// Every authenticated read class needs the fence. Mutation-gate eligibility
		// deliberately excludes delegated and remote readers and is not read admission.
		switch s.requestAuthentication(r).Mode {
		case AuthModeLoopback, AuthModeAPIKey, AuthModeSession, AuthModeCaller, AuthModeDelegated, AuthModeRemoteClient:
		default:
			next.ServeHTTP(w, r)
			return
		}
		release, err := s.contentReadGuard(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "x_content_cleanup_pending", "X content cleanup is incomplete")
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// InvalidateMessageContent is used under the daemon's drained content write
// fence. Draining is essential: forgetting a singleflight key does not cancel
// a parser that could otherwise repopulate an evicted entry.
func (s *Server) InvalidateMessageContent(ids []int64) {
	if s == nil || s.inlineCache == nil {
		return
	}
	c := s.inlineCache
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if el, ok := c.entries[id]; ok {
			item := cacheItem(el)
			c.totalBytes -= item.entry.bytes
			delete(c.entries, id)
			c.order.Remove(el)
		}
	}
}

// RetireContentAnalytics runs under the drained content write fence. It closes
// every daemon-managed DuckDB handle and owned spill directory, then serves
// SQL-backed reads until restart. A late background cache initializer cannot
// republish a handle opened before erasure.
func (s *Server) RetireContentAnalytics(fallback query.Engine, mode string) error {
	s.contentAnalyticsMu.Lock()
	defer s.contentAnalyticsMu.Unlock()
	s.contentAnalyticsRetired = true
	s.analyticsState.Store(&analyticsEngineState{engine: fallback, mode: mode})
	var err error
	var failed []*query.DuckDBEngine
	for _, engine := range s.contentAnalyticsEngines {
		closeErr := engine.Close()
		if closeErr != nil {
			failed = append(failed, engine)
		}
		err = errors.Join(err, closeErr)
	}
	// A failed owned-directory removal must remain retryable even though the
	// underlying DuckDB connection has already closed.
	s.contentAnalyticsEngines = failed
	return err
}
