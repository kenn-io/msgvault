package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type senderAuthDiagnosticStore interface {
	DB() *sql.DB
	IsPostgreSQL() bool
	GetSourceByIDContext(ctx context.Context, id int64) (*store.Source, error)
}

type senderAuthDiagnosticsInput struct {
	SourceID int64 `query:"source_id" minimum:"1" required:"true" doc:"Archive-local source ID to sample."`
	Limit    int   `query:"limit" default:"100" minimum:"1" maximum:"1000" doc:"Maximum newest messages to inspect; counts describe this sample only."`
	Suggest  bool  `query:"suggest" doc:"Include untrusted observed receiver hostnames for manual review."`
}
type senderAuthDiagnosticsOutput struct{ Body query.SenderAuthDiagnostics }

func (s *Server) registerSenderAuthDiagnostics(api huma.API) {
	// This operation is deliberately absent from delegated and remote allowlists.
	// Authentication middleware rejects those callers before archive access.
	huma.Register(api, withAPIKeySecurity(huma.Operation{OperationID: "getSenderAuthDiagnostics", Method: http.MethodGet, Path: "/sender-auth/diagnostics", Summary: "Inspect a bounded sample of sender receipt metadata", Tags: []string{"Messages"}, Errors: []int{http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable}}), func(ctx context.Context, input *senderAuthDiagnosticsInput) (*senderAuthDiagnosticsOutput, error) {
		st, ok := s.store.(senderAuthDiagnosticStore)
		if !ok || st.DB() == nil {
			return nil, huma.Error503ServiceUnavailable("Archive diagnostics unavailable")
		}
		exists, err := st.GetSourceByIDContext(ctx, input.SourceID)
		if errors.Is(err, store.ErrSourceNotFound) || (err == nil && exists == nil) {
			return nil, huma.Error404NotFound("Source not found")
		}
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("Source lookup unavailable")
		}
		e := query.NewEngineWithDialect(st.DB(), query.SQLiteQueryDialect{})
		if st.IsPostgreSQL() {
			e = query.NewEngineWithDialect(st.DB(), query.PostgreSQLQueryDialect{})
		}
		e.SetSenderAuthPins(s.senderAuthPins)
		result, err := e.DiagnoseSenderAuth(ctx, input.SourceID, input.Limit, input.Suggest)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("Receipt diagnostics unavailable")
		}
		return &senderAuthDiagnosticsOutput{Body: *result}, nil
	})
}
