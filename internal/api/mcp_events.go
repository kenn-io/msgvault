package api

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/mcpevents"
)

func (s *Server) registerMCPEventsRoutes(api huma.API) {
	registerAPIV1RawHumaJSONRoute[mcpevents.ListResult](api, "mcpEventsList", http.MethodGet, "/mcp-events/list", "List owner MCP Events", s.handleMCPEventsList)
	registerAPIV1RawHumaJSONRouteWithRequest[mcpevents.SubscribeRequest, mcpevents.SubscribeResult](api, "mcpEventsSubscribe", http.MethodPost, "/mcp-events/subscribe", "Subscribe to owner MCP Events", s.handleMCPEventsSubscribe)
	registerAPIV1RawHumaJSONRouteWithRequest[mcpevents.UnsubscribeRequest, mcpevents.UnsubscribeResult](api, "mcpEventsUnsubscribe", http.MethodPost, "/mcp-events/unsubscribe", "Unsubscribe from owner MCP Events", s.handleMCPEventsUnsubscribe)
}
func eventsError(w http.ResponseWriter, err error) {
	var e *mcpevents.Error
	if !errors.As(err, &e) {
		e = &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "unavailable"}
	}
	status := http.StatusServiceUnavailable
	switch e.Code {
	case mcpevents.InvalidParams:
		status = http.StatusBadRequest
	case mcpevents.NotFound:
		status = http.StatusNotFound
	case mcpevents.Forbidden:
		status = http.StatusForbidden
	case mcpevents.Unsupported:
		status = http.StatusBadRequest
	case mcpevents.ResourceExhausted:
		status = http.StatusTooManyRequests
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, e)
}
func (s *Server) handleMCPEventsList(w http.ResponseWriter, r *http.Request) {
	enabled := s.mcpEvents.Available(r.Context())
	events := []mcpevents.Definition{}
	if enabled {
		events = mcpevents.Definitions()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, mcpevents.ListResult{Enabled: enabled, Events: events})
}
func readEventsBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
}
func (s *Server) handleMCPEventsSubscribe(w http.ResponseWriter, r *http.Request) {
	raw, err := readEventsBody(r)
	if err != nil {
		eventsError(w, &mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
		return
	}
	in, err := mcpevents.DecodeSubscribe(raw)
	if err != nil {
		eventsError(w, err)
		return
	}
	release, err := s.beginMutation("MCP Events")(r.Context())
	if err != nil {
		eventsError(w, err)
		return
	}
	defer release()
	result, err := s.mcpEvents.Subscribe(r.Context(), in)
	if err != nil {
		eventsError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, result)
}
func (s *Server) handleMCPEventsUnsubscribe(w http.ResponseWriter, r *http.Request) {
	raw, err := readEventsBody(r)
	if err != nil {
		eventsError(w, &mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
		return
	}
	in, err := mcpevents.DecodeUnsubscribe(raw)
	if err != nil {
		eventsError(w, err)
		return
	}
	release, err := s.beginMutation("MCP Events")(r.Context())
	if err == nil {
		defer release()
		err = s.mcpEvents.Unsubscribe(r.Context(), in)
	}
	if err != nil {
		eventsError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}
