package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/wakeups"
)

func (s *Server) registerMCPEventsRoutes(api huma.API) {
	registerAPIV1RawHumaJSONRoute[mcpevents.MCPEventsListResult](api, "mcpEventsList", http.MethodGet, "/mcp-events/list", "List MCP Events", s.handleMCPEventsList)
	registerMCPEventsMutation[mcpevents.MCPEventsSubscribeRequest, mcpevents.MCPEventsSubscribeResult](api, "mcpEventsSubscribe", http.MethodPost, "/mcp-events/subscribe", "Subscribe to MCP Events", s.handleMCPEventsSubscribe)
	registerMCPEventsMutation[mcpEventsUnsubscribeBody, mcpevents.MCPEventsUnsubscribeResult](api, "mcpEventsUnsubscribe", http.MethodPost, "/mcp-events/unsubscribe", "Unsubscribe from MCP Events", s.handleMCPEventsUnsubscribe)
}

// mcpEventsUnsubscribeBody describes the URL-only unsubscribe wire contract.
type mcpEventsUnsubscribeBody struct {
	Name      string                       `json:"name"`
	Arguments mcpevents.MCPEventsArguments `json:"arguments"`
	Delivery  struct {
		Mode   string `json:"mode,omitempty"`
		URL    string `json:"url"`
		Secret string `json:"secret,omitempty"`
	} `json:"delivery"`
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

// eventsAdmissionError keeps the safe source-scope reason of a pre-service
// admission failure inside the Events error envelope; other API errors pass
// through unchanged.
func eventsAdmissionError(err *apiHTTPError) error {
	if err.ErrorResponse.Error == "source_scope_unavailable" {
		return &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "source_scope_unavailable"}
	}
	return err
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
	serveEventsMutation(w, r, s, mcpevents.DecodeSubscribe, func(ctx context.Context, in mcpevents.MCPEventsSubscribeRequest, release *func()) (mcpevents.MCPEventsSubscribeResult, error) {
		service := s.mcpEvents
		owner := "owner"
		var authorize func(context.Context, bool) error
		grant := s.requestAuthentication(r).Grant
		if grant != nil {
			release, apiErr := s.beginAgentRead(r, "listChangedMessages", grant, agentgrant.PermissionSearchRead)
			if apiErr != nil {
				return mcpevents.MCPEventsSubscribeResult{}, eventsAdmissionError(apiErr)
			}
			ids := agentReadSourceIDs(r)
			if in.Arguments.SourceID != 0 && !slices.Contains(ids, in.Arguments.SourceID) {
				release()
				return mcpevents.MCPEventsSubscribeResult{}, &mcpevents.Error{Code: mcpevents.Forbidden, Reason: "permission_denied"}
			}
			authorize = func(ctx context.Context, creating bool) error {
				request := r.Clone(ctx)
				done, apiErr := s.beginAgentRead(request, "listChangedMessages", grant, agentgrant.PermissionSearchRead)
				if apiErr != nil {
					return &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "source_scope_unavailable"}
				}
				defer done()
				ids := agentReadSourceIDs(request)
				if in.Arguments.SourceID != 0 && !slices.Contains(ids, in.Arguments.SourceID) {
					return &mcpevents.Error{Code: mcpevents.Forbidden, Reason: "permission_denied"}
				}
				if !creating || in.Arguments.PersonID == 0 {
					return nil
				}
				filter, resolveErr := changefeed.ResolveFilter(request.Context(), s.store, changefeed.Filter{PersonID: in.Arguments.PersonID})
				switch {
				case errors.Is(resolveErr, store.ErrPersonNotFound), errors.Is(resolveErr, resolver.ErrEmptyPopulation):
					return &mcpevents.Error{Code: mcpevents.Forbidden, Reason: "permission_denied"}
				case resolveErr != nil || service.Store == nil:
					return &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "source_scope_unavailable"}
				}
				inside, outside, err := service.Store.PersonSourceEvidence(request.Context(), filter.Participants, ids)
				switch {
				case err != nil:
					return &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "source_scope_unavailable"}
				case !inside:
					return &mcpevents.Error{Code: mcpevents.Forbidden, Reason: "permission_denied"}
				case outside:
					return &mcpevents.Error{Code: mcpevents.Forbidden, Reason: "person_scope_exceeds_grant"}
				}
				return nil
			}
			release()
			owner = agentgrant.Owner(grant.ID)
		}
		verify := func(ctx context.Context, endpoint, id string, secret []byte) error {
			(*release)()
			*release = nil
			var verifyErr error
			if s.verifyEventsCallback != nil {
				verifyErr = s.verifyEventsCallback(ctx, endpoint, id, secret)
			} else if category := wakeups.VerifyEventCallback(ctx, endpoint, id, secret); category != "" {
				verifyErr = &mcpevents.Error{Code: mcpevents.CallbackEndpoint, Reason: category}
			}
			if verifyErr != nil {
				return verifyErr
			}
			var err error
			*release, err = s.beginMutation("MCP Events")(ctx)
			if err != nil {
				return &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "unavailable"}
			}
			return nil
		}
		return service.Subscribe(ctx, in, owner, verify, authorize)
	})
}
func (s *Server) handleMCPEventsUnsubscribe(w http.ResponseWriter, r *http.Request) {
	serveEventsMutation(w, r, s, mcpevents.DecodeUnsubscribe, func(ctx context.Context, in mcpevents.MCPEventsUnsubscribeRequest, _ *func()) (mcpevents.MCPEventsUnsubscribeResult, error) {
		owner := "owner"
		if grant := s.requestAuthentication(r).Grant; grant != nil {
			owner = agentgrant.Owner(grant.ID)
		}
		return mcpevents.MCPEventsUnsubscribeResult{}, s.mcpEvents.Unsubscribe(ctx, in, owner)
	})
}
func serveEventsMutation[Req, Resp any](w http.ResponseWriter, r *http.Request, s *Server, decode func([]byte) (Req, error), run func(context.Context, Req, *func()) (Resp, error)) {
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
	ctx := r.Context()
	release, err := s.beginMutation("MCP Events")(ctx)
	if err != nil {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	result, err := run(ctx, in, &release)
	if err != nil {
		if apiErr, ok := errors.AsType[*apiHTTPError](err); ok {
			writeAPIHTTPError(w, apiErr)
		} else {
			eventsError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}
