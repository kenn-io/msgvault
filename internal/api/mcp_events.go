package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/mcpevents"
)

func (s *Server) registerMCPEventsRoutes(api huma.API) {
	registerAPIV1RawHumaJSONRoute[mcpevents.MCPEventsListResult](api, "mcpEventsList", http.MethodGet, "/mcp-events/list", "List owner MCP Events", s.handleMCPEventsList)
	registerMCPEventsMutation[mcpevents.MCPEventsSubscribeRequest, mcpevents.MCPEventsSubscribeResult](api, "mcpEventsSubscribe", http.MethodPost, "/mcp-events/subscribe", "Subscribe to owner MCP Events", s.handleMCPEventsSubscribe)
	registerMCPEventsMutation[mcpevents.MCPEventsUnsubscribeRequest, mcpevents.MCPEventsUnsubscribeResult](api, "mcpEventsUnsubscribe", http.MethodPost, "/mcp-events/unsubscribe", "Unsubscribe from owner MCP Events", s.handleMCPEventsUnsubscribe)
}
func registerMCPEventsMutation[Req any, Resp any](api huma.API, operationID, method, path, summary string, handler http.HandlerFunc) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.RequestBody = jsonRequestBodyFor[Req](api)
	op.Responses = jsonResponsesFor[Resp](api)
	op.Responses["default"] = &huma.Response{
		Description: "Error",
		Content: map[string]*huma.MediaType{applicationJSONMediaType: {Schema: &huma.Schema{AnyOf: []*huma.Schema{
			schemaFor[mcpevents.Error](api), schemaFor[ErrorResponse](api),
		}}}},
	}
	registerRawHumaRoute(api, op, handler)
}
func eventsError(w http.ResponseWriter, err error) {
	var e *mcpevents.Error
	if !errors.As(err, &e) {
		e = &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "unavailable"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status())
	_ = json.MarshalWrite(w, e)
}
func (s *Server) handleMCPEventsList(w http.ResponseWriter, r *http.Request) {
	enabled := s.mcpEvents.Available(r.Context())
	events := []mcpevents.MCPEventsDefinition{}
	if enabled {
		events = mcpevents.Definitions()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, mcpevents.MCPEventsListResult{Enabled: enabled, Events: events})
}
func readEventsBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
}
func (s *Server) handleMCPEventsSubscribe(w http.ResponseWriter, r *http.Request) {
	serveEventsMutation(w, r, s, mcpevents.DecodeSubscribe, s.mcpEvents.Subscribe)
}
func (s *Server) handleMCPEventsUnsubscribe(w http.ResponseWriter, r *http.Request) {
	serveEventsMutation(w, r, s, mcpevents.DecodeUnsubscribe, func(ctx context.Context, in mcpevents.MCPEventsUnsubscribeRequest) (mcpevents.MCPEventsUnsubscribeResult, error) {
		return mcpevents.MCPEventsUnsubscribeResult{}, s.mcpEvents.Unsubscribe(ctx, in)
	})
}
func serveEventsMutation[Req, Resp any](w http.ResponseWriter, r *http.Request, s *Server, decode func([]byte) (Req, error), run func(context.Context, Req) (Resp, error)) {
	raw, err := readEventsBody(r)
	if err != nil {
		eventsError(w, &mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
		return
	}
	in, err := decode(raw)
	if err != nil {
		eventsError(w, err)
		return
	}
	release, err := s.beginMutation("MCP Events")(r.Context())
	if err != nil {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer release()
	result, err := run(r.Context(), in)
	if err != nil {
		eventsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
