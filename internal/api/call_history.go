package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
)

const callImportPath = "/api/v1/import/calls"

type CallHistoryStore interface {
	GetOrCreateSourceContext(ctx context.Context, sourceType, identifier string) (*store.Source, error)
	PutCallObservationContext(ctx context.Context, sourceID int64, observation callhistory.Observation) (int64, error)
	ListCallObservationsContext(ctx context.Context, q store.CallQuery) ([]store.CallObservationView, error)
	SummarizeCallsContext(ctx context.Context, q store.CallQuery) ([]store.PersonCallSummary, error)
}

type CallImportResponse struct {
	MessageID int64 `json:"message_id"`
	SourceID  int64 `json:"source_id"`
}
type CallEvidenceResponse struct {
	Items []store.CallObservationView `json:"items"`
}
type CallSummaryResponse struct {
	Items []store.PersonCallSummary `json:"items"`
}
type CallImportRequest callhistory.ImportRequest
type CallParticipant callhistory.Participant

func (s *Server) registerCallHistoryRoutes(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	registry.RegisterTypeAlias(reflect.TypeFor[callhistory.ImportRequest](), reflect.TypeFor[CallImportRequest]())
	registry.RegisterTypeAlias(reflect.TypeFor[callhistory.Participant](), reflect.TypeFor[CallParticipant]())
	op := rawAPIV1Operation("importCallObservation", http.MethodPost, "/import/calls", "Import one supplied call or scheduled meeting observation")
	op.RequestBody = jsonRequestBodyFor[callhistory.ImportRequest](api)
	// jsontext.Value is raw provider JSON, not a base64-encoded byte string.
	registry.Map()["CallImportRequest"].Properties["record"] = &huma.Schema{Type: "object", AdditionalProperties: true}
	op.Responses = jsonResponsesFor[CallImportResponse](api)
	op.Errors = []int{400, 401, 409, 413, 415, 422, 500, 503}
	registerRawHumaRoute(api, op, s.handleCallImport)
	for _, summary := range []bool{false, true} {
		id, path, title := "listCallObservations", "/calls", "Inspect call and scheduled meeting observations"
		if summary {
			id, path, title = "summarizeCalls", "/calls/summary", "Count call evidence per person"
		}
		op := rawAPIV1Operation(id, http.MethodGet, path, title)
		op.Responses = jsonResponsesFor[CallEvidenceResponse](api)
		if summary {
			op.Responses = jsonResponsesFor[CallSummaryResponse](api)
		}
		pagination := "after_id"
		if summary {
			pagination = "offset"
		}
		for _, name := range []string{"person_id", "limit", pagination} {
			op.Parameters = append(op.Parameters, &huma.Param{Name: name, In: "query", Schema: &huma.Schema{Type: "integer", Format: "int64"}})
		}
		for _, name := range []string{"after", "before"} {
			op.Parameters = append(op.Parameters, &huma.Param{Name: name, In: "query", Schema: &huma.Schema{Type: "string", Format: "date-time"}})
		}
		op.Errors = []int{400, 401, 500, 503}
		registerRawHumaRoute(api, op, func(w http.ResponseWriter, r *http.Request) { s.handleCallRead(w, r, summary) })
	}
}

func (s *Server) writeCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, callhistory.ErrImportTooLarge):
		writeError(w, 413, "request_too_large", "Call import exceeds 1 MiB")
	case errors.Is(err, callhistory.ErrMalformedImport):
		writeError(w, 400, "bad_request", "Invalid call import JSON")
	case errors.Is(err, callhistory.ErrInvalid):
		writeError(w, 422, "validation_failed", "Call observation failed validation")
	case errors.Is(err, store.ErrCallObservationWithdrawn), errors.Is(err, store.ErrSourceRetired):
		writeError(w, 409, "evidence_withdrawn", "Source or observation was withdrawn")
	default:
		if !s.writeIfContextError(w, err) {
			writeError(w, 500, "internal_error", "Call evidence operation failed")
		}
	}
}

func (s *Server) handleCallImport(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != applicationJSONMediaType {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	o, err := callhistory.DecodeImport(r.Body)
	if err != nil {
		s.writeCallError(w, err)
		return
	}
	st, ok := s.store.(CallHistoryStore)
	if !ok {
		writeError(w, 503, "service_unavailable", "Call history is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(ctx, operationGateLabelFromPath(callImportPath))
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	source, err := st.GetOrCreateSourceContext(r.Context(), callhistory.SourceType, o.SourceIdentifier())
	if err != nil {
		s.writeCallError(w, err)
		return
	}
	id, err := st.PutCallObservationContext(r.Context(), source.ID, o)
	if err != nil {
		s.writeCallError(w, err)
		return
	}
	writeJSON(w, 200, CallImportResponse{MessageID: id, SourceID: source.ID})
}

func callQueryFromRequest(r *http.Request) (store.CallQuery, error) {
	var q store.CallQuery
	values := r.URL.Query()
	for name, target := range map[string]*int64{"person_id": &q.PersonID, "after_id": &q.AfterID} {
		if value := values.Get(name); value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return q, fmt.Errorf("parse %s: %w", name, err)
			}
			*target = n
		}
	}
	for name, target := range map[string]*int{"limit": &q.Limit, "offset": &q.Offset} {
		if value := values.Get(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return q, fmt.Errorf("parse %s: %w", name, err)
			}
			*target = n
		}
	}
	for name, target := range map[string]**time.Time{"after": &q.After, "before": &q.Before} {
		if value := values.Get(name); value != "" {
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return q, fmt.Errorf("parse %s: %w", name, err)
			}
			*target = &at
		}
	}
	return q, nil
}

func (s *Server) handleCallRead(w http.ResponseWriter, r *http.Request, summary bool) {
	q, err := callQueryFromRequest(r)
	if err != nil {
		writeError(w, 400, "bad_request", "Invalid call query")
		return
	}
	st, ok := s.store.(CallHistoryStore)
	if !ok {
		writeError(w, 503, "service_unavailable", "Call history is unavailable")
		return
	}
	var response any
	if summary {
		var items []store.PersonCallSummary
		items, err = st.SummarizeCallsContext(r.Context(), q)
		response = CallSummaryResponse{Items: items}
	} else {
		var items []store.CallObservationView
		items, err = st.ListCallObservationsContext(r.Context(), q)
		response = CallEvidenceResponse{Items: items}
	}
	if errors.Is(err, callhistory.ErrInvalid) {
		writeError(w, 400, "bad_request", "Invalid call query bounds")
	} else if err != nil {
		s.writeCallError(w, err)
	} else {
		writeJSON(w, 200, response)
	}
}
