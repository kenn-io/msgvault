package api

import (
	"context"
	"net/http"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func sourceScopedOperation(r *http.Request, operation Operation) bool {
	if operation.Method != http.MethodGet {
		return false
	}
	switch operation.ID {
	case "listMessages", "getMessage", "getConversation", "getAttachment", "listCLIAccounts":
		return true
	case "searchMessages":
		mode := r.URL.Query().Get("mode")
		return mode == "" || mode == "fts"
	default:
		return false
	}
}

// beginCallerRead keeps source selection and subsequent object hydration in
// one snapshot. The host's separate authorization transaction stays host-owned.
func (s *Server) beginCallerRead(r *http.Request) (func(), error) {
	reader, ok := s.store.(*store.Store)
	if !ok {
		return nil, newAPIHTTPError(http.StatusServiceUnavailable, "source_scope_unavailable", "Source authorization is unavailable")
	}
	ctx, release, err := reader.BeginReadSnapshotContext(r.Context())
	if err != nil {
		return nil, err
	}
	if boundary, ok := r.Context().Value(readResponseKey{}).(*readResponseWriter); ok {
		boundary.release = release
	}
	if s.queryEngineForContext(ctx) != nil {
		engine := query.NewEngine(reader.DB(), reader.IsPostgreSQL())
		ctx = context.WithValue(ctx, analyticsEngineContextKey{}, &analyticsEngineState{engine: engine, mode: AnalyticsModeSQL})
	}
	*r = *r.WithContext(ctx)
	return release, nil
}

func (s *Server) sourceScopedRead(r *http.Request) bool {
	return sourcescope.Restricted(r.Context()) || s.requestAuthentication(r).Grant != nil
}
