package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/mcpevents"
)

const mcpEventsPath = "/api/v1/mcp/events"

var mcpEventsOperationIDs = map[string]bool{
	"listMCPEvents": true, "subscribeMCPEvents": true, "unsubscribeMCPEvents": true,
	"getMCPEventsStatus": true, "listMCPCalendarSources": true,
	"getMCPEvent": true, "getMCPEventMessage": true,
}

type mcpEventsRequestKey struct{}
type MCPEventsListRequest struct{}
type MCPEventsErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// Shared authentication and rate-limit middleware uses error/message only.
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SetMCPEvents installs the daemon-owned service before serving requests.
func (s *Server) SetMCPEvents(service *mcpevents.Service) {
	s.mcpEvents = service
	s.mcpEventsUnavailable = nil
}

// SetMCPEventsUnavailable preserves only a fixed startup failure for owner status.
func (s *Server) SetMCPEventsUnavailable(err error) {
	s.mcpEvents = nil
	s.mcpEventsUnavailable = &mcpevents.Error{Code: -32015, Reason: "events_storage_unavailable"}
	if candidate, ok := errors.AsType[*mcpevents.Error](err); ok {
		if safe, allowed := mcpevents.SafeError(candidate.Code, candidate.Reason); allowed {
			s.mcpEventsUnavailable = safe
		}
	}
}

// MCPEventsOperation holds the archive gate only for a Store command. The
// service resolves and contacts callbacks outside this function.
func (s *Server) MCPEventsOperation(ctx context.Context, operation func() error) error {
	var release func()
	var acquired bool
	if requested, _ := ctx.Value(mcpEventsRequestKey{}).(bool); requested {
		release, acquired = s.beginLabeledOperationGateWork(ctx, "MCP Events")
	} else {
		release, acquired = s.beginBackgroundOperationGateWork(ctx, "MCP Events")
	}
	if !acquired {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("events_storage_unavailable")
	}
	defer release()
	return operation()
}

// MCPEventsDeliveryOperation counts commit-triggered delivery as request work
// so resumable scheduled jobs yield while a live occurrence is delivered.
func (s *Server) MCPEventsDeliveryOperation(ctx context.Context, operation func() error) error {
	release, acquired := s.beginLabeledOperationGateWork(ctx, "MCP Events delivery")
	if !acquired {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("events_storage_unavailable")
	}
	defer release()
	return operation()
}

func (s *Server) registerMCPEventsRoutes(api huma.API) {
	list := rawAPIV1Operation("listMCPEvents", http.MethodPost, "/mcp/events/list", "List scoped MCP Events")
	list.RequestBody = jsonRequestBodyFor[MCPEventsListRequest](api)
	list.Responses = jsonResponsesFor[mcpevents.ListResult](api)
	registerMCPEventsRoute(api, list, s.handleMCPEventsList)
	subscribe := rawAPIV1Operation("subscribeMCPEvents", http.MethodPost, "/mcp/events/subscribe", "Verify and subscribe an owner callback")
	subscribe.RequestBody = jsonRequestBodyFor[mcpevents.SubscribeRequest](api)
	subscribe.Responses = jsonResponsesFor[mcpevents.SubscribeResult](api)
	registerMCPEventsRoute(api, subscribe, s.handleMCPEventsSubscribe)
	unsubscribe := rawAPIV1Operation("unsubscribeMCPEvents", http.MethodPost, "/mcp/events/unsubscribe", "End an owner MCP Events subscription")
	unsubscribe.RequestBody = jsonRequestBodyFor[mcpevents.UnsubscribeRequest](api)
	unsubscribe.Responses = rawHumaResponses(http.StatusNoContent)
	registerMCPEventsRoute(api, unsubscribe, s.handleMCPEventsUnsubscribe)
	status := rawAPIV1Operation("getMCPEventsStatus", http.MethodGet, "/mcp/events/status", "Get owner MCP Events delivery status")
	status.Responses = jsonResponsesFor[[]mcpevents.SubscriptionStatus](api)
	registerMCPEventsRoute(api, status, s.handleMCPEventsStatus)
	calendars := rawAPIV1Operation("listMCPCalendarSources", http.MethodGet, "/mcp/events/calendar-sources", "List subscribable archive calendars")
	calendars.Responses = jsonResponsesFor[[]mcpevents.CalendarSource](api)
	registerMCPEventsRoute(api, calendars, s.handleMCPCalendarSources)
	getEvent := rawAPIV1Operation("getMCPEvent", http.MethodGet, "/mcp/events/event", "Recover a retained MCP occurrence")
	getEvent.Parameters = []*huma.Param{{Name: "event_id", In: "query", Required: true, Schema: &huma.Schema{Type: "string"}}}
	getEvent.Responses = jsonResponsesFor[mcpevents.Envelope](api)
	registerMCPEventsRoute(api, getEvent, s.handleMCPEvent)
	getMessage := rawAPIV1Operation("getMCPEventMessage", http.MethodGet, "/mcp/events/messages/{id}", "Read a message authorized by a retained MCP occurrence")
	getMessage.Parameters = []*huma.Param{{Name: "id", In: "path", Required: true, Schema: &huma.Schema{Type: "string", Pattern: `^[1-9][0-9]{0,18}$`}}, {Name: "event_id", In: "query", Required: true, Schema: &huma.Schema{Type: "string"}}}
	getMessage.Responses = jsonResponsesFor[cliMessageResponse](api)
	registerMCPEventsRoute(api, getMessage, s.handleMCPEventMessage)
}

func registerMCPEventsRoute(api huma.API, op huma.Operation, handler http.HandlerFunc) {
	op.Responses["default"] = &huma.Response{Description: "Error", Content: map[string]*huma.MediaType{applicationJSONMediaType: {Schema: schemaFor[MCPEventsErrorResponse](api)}}}
	registerRawHumaRoute(api, op, handler)
}

func (s *Server) mcpEventsOwner(w http.ResponseWriter, r *http.Request) (*mcpevents.Service, string, bool) {
	if s.requestAuthentication(r).Mode != AuthModeAPIKey || s.mcpEventsPrincipal == "" {
		writeMCPEventsError(w, &mcpevents.Error{Code: -32012, Reason: "owner_required"})
		return nil, "", false
	}
	if s.mcpEventsUnavailable != nil {
		writeMCPEventsError(w, s.mcpEventsUnavailable)
		return nil, "", false
	}
	if s.mcpEvents == nil || len(s.mcpEvents.Catalog().Events) == 0 {
		writeMCPEventsError(w, &mcpevents.Error{Code: -32015, Reason: "events_unavailable"})
		return nil, "", false
	}
	return s.mcpEvents, s.mcpEventsPrincipal, true
}

func decodeMCPEventsRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	trimmed := strings.TrimSpace(string(data))
	if err != nil || len(data) > 1<<20 || !strings.HasPrefix(trimmed, "{") || json.Unmarshal(data, target, json.RejectUnknownMembers(true)) != nil {
		writeMCPEventsError(w, &mcpevents.Error{Code: -32602, Reason: "invalid_request"})
		return false
	}
	return true
}

func writeMCPEventsError(w http.ResponseWriter, err error) {
	safe := &mcpevents.Error{Code: -32015, Reason: "events_storage_unavailable"}
	if candidate, ok := errors.AsType[*mcpevents.Error](err); ok {
		if checked, allowed := mcpevents.SafeError(candidate.Code, candidate.Reason); allowed {
			safe = checked
		}
	}
	status := http.StatusServiceUnavailable
	switch safe.Code {
	case -32602, -32014:
		status = http.StatusBadRequest
		if safe.Reason == "event_unavailable" || safe.Reason == "message_unavailable" {
			status = http.StatusNotFound
		}
	case -32012:
		status = http.StatusForbidden
	case -32013:
		status = http.StatusConflict
		if safe.Reason == "subscription_limit" {
			status = http.StatusTooManyRequests
		}
	}
	writeJSON(w, status, MCPEventsErrorResponse{Error: "mcp_events_error", Message: safe.Reason, Code: safe.Code, Reason: safe.Reason})
}

func mcpEventsRequestContext(r *http.Request) context.Context {
	return context.WithValue(r.Context(), mcpEventsRequestKey{}, true)
}

func (s *Server) handleMCPEventsList(w http.ResponseWriter, r *http.Request) {
	service, _, ok := s.mcpEventsOwner(w, r)
	if !ok {
		return
	}
	var request MCPEventsListRequest
	if !decodeMCPEventsRequest(w, r, &request) {
		return
	}
	writeJSON(w, http.StatusOK, service.Catalog())
}

func (s *Server) handleMCPEventsSubscribe(w http.ResponseWriter, r *http.Request) {
	service, principal, ok := s.mcpEventsOwner(w, r)
	if !ok {
		return
	}
	var request mcpevents.SubscribeRequest
	if !decodeMCPEventsRequest(w, r, &request) {
		return
	}
	result, err := service.Subscribe(mcpEventsRequestContext(r), principal, request)
	if err != nil {
		writeMCPEventsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMCPEventsUnsubscribe(w http.ResponseWriter, r *http.Request) {
	service, principal, ok := s.mcpEventsOwner(w, r)
	if !ok {
		return
	}
	var request mcpevents.UnsubscribeRequest
	if !decodeMCPEventsRequest(w, r, &request) {
		return
	}
	if err := service.Unsubscribe(mcpEventsRequestContext(r), principal, request); err != nil {
		writeMCPEventsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMCPEventsStatus(w http.ResponseWriter, r *http.Request) {
	service, principal, ok := s.mcpEventsOwner(w, r)
	if !ok {
		return
	}
	rows, err := service.Status(mcpEventsRequestContext(r), principal)
	if err != nil {
		writeMCPEventsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleMCPCalendarSources(w http.ResponseWriter, r *http.Request) {
	service, principal, ok := s.mcpEventsOwner(w, r)
	if !ok {
		return
	}
	rows, err := service.CalendarSources(mcpEventsRequestContext(r), principal)
	if err != nil {
		writeMCPEventsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleMCPEvent(w http.ResponseWriter, r *http.Request) {
	service, principal, ok := s.mcpEventsOwner(w, r)
	if !ok {
		return
	}
	values := r.URL.Query()["event_id"]
	if len(values) != 1 {
		writeMCPEventsError(w, &mcpevents.Error{Code: -32602, Reason: "invalid_event_id"})
		return
	}
	event, err := service.GetEvent(mcpEventsRequestContext(r), principal, values[0])
	if err != nil {
		writeMCPEventsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *Server) handleMCPEventMessage(w http.ResponseWriter, r *http.Request) {
	service, principal, ok := s.mcpEventsOwner(w, r)
	if !ok {
		return
	}
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	values := r.URL.Query()["event_id"]
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw || len(values) != 1 {
		writeMCPEventsError(w, &mcpevents.Error{Code: -32602, Reason: "invalid_identifier"})
		return
	}
	message, err := service.GetMessage(mcpEventsRequestContext(r), principal, values[0], id)
	if err != nil {
		writeMCPEventsError(w, err)
		return
	}
	if message == nil {
		writeMCPEventsError(w, &mcpevents.Error{Code: -32602, Reason: "message_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, cliMessageResponseFromQuery(message))
}
