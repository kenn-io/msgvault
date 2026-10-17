package api

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/loops"
)

type CloseOpenLoopRequest struct {
	Note string `json:"note,omitempty"`
}

type SnoozeOpenLoopRequest struct {
	Until time.Time `json:"until"`
}

func (s *Server) registerLoopRoutes(api huma.API) {
	type route struct {
		id, method, path, summary string
		responses                 map[string]*huma.Response
		body                      *huma.RequestBody
	}
	routes := []route{
		{"listOpenLoops", http.MethodGet, "/loops", "List open-loop history", jsonResponsesFor[loops.ListResponse](api), nil},
		{"getWaitingLoops", http.MethodGet, "/loops/waiting", "Rank people waiting for the owner's reply", jsonResponsesFor[loops.WaitingResponse](api), nil},
		{"getOpenLoop", http.MethodGet, "/loops/{id}", "Get one loop and archived evidence references", jsonResponsesFor[loops.DetailResponse](api), nil},
		{"closeOpenLoop", http.MethodPost, "/loops/{id}/done", "Mark an unanswered turn done", jsonResponsesFor[loops.DetailResponse](api), jsonRequestBodyFor[CloseOpenLoopRequest](api)},
		{"snoozeOpenLoop", http.MethodPost, "/loops/{id}/snooze", "Snooze an open loop", jsonResponsesFor[loops.DetailResponse](api), jsonRequestBodyFor[SnoozeOpenLoopRequest](api)},
		{"unsnoozeOpenLoop", http.MethodDelete, "/loops/{id}/snooze", "Remove a loop's snooze", jsonResponsesFor[loops.DetailResponse](api), nil},
		{"listLoopSuppressions", http.MethodGet, "/loops/suppressions", "List owner loop suppressions", jsonResponsesFor[loops.SuppressionListResponse](api), nil},
		{"createLoopSuppression", http.MethodPost, "/loops/suppressions", "Mute a loop target or preview affected loops", jsonResponsesFor[loops.SuppressionResult](api, http.StatusOK, http.StatusCreated), jsonRequestBodyFor[loops.SuppressionRequest](api)},
		{"deleteLoopSuppression", http.MethodDelete, "/loops/suppressions/{id}", "Unmute a suppressed target", rawHumaResponses(http.StatusNoContent), nil},
		{"getLoopsStatus", http.MethodGet, "/loops/status", "Get certified loop progress and archive freshness", jsonResponsesFor[loops.StatusResponse](api), nil},
		{"runLoops", http.MethodPost, "/loops/run", "Request one bounded derivation pass", jsonResponsesFor[loops.RunResponse](api), nil},
		{"rebuildLoops", http.MethodPost, "/loops/rebuild", "Request a preserving loop rebuild", jsonResponsesFor[loops.RunResponse](api), nil},
		{"listPersonLoops", http.MethodGet, "/people/{id}/loops", "List a person's open loops", jsonResponsesFor[loops.ListResponse](api), nil},
	}
	for _, r := range routes {
		op := rawAPIV1Operation(r.id, r.method, r.path, r.summary)
		op.Parameters = loopRouteParameters(r.id)
		op.RequestBody = r.body
		op.Responses = r.responses
		op.Errors = []int{400, 401, 403, 404, 409, 413, 415, 500, 503}
		addErrorResponses(api, op.Responses, op.Errors...)
		if r.id == "getWaitingLoops" {
			op.Responses["409"] = jsonResponsesFor[loops.LoopsStaleResponse](api, http.StatusConflict)["409"]
		}
		if r.method == http.MethodDelete {
			documentRawHumaOperation(api, &op)
		} else {
			registerRawHumaRoute(api, op, s.loopHandler(r.id))
		}
	}
	s.registerLoopDeletes(api)
}

// ServeMux cannot register both /loops/{id}/snooze and
// /loops/suppressions/{id}: their shapes overlap without either being more
// specific. Document both public operations, then dispatch their two shapes
// through one owner-only mutation route. The internal dispatch operation is
// never documented or advertised as a client capability.
func (s *Server) registerLoopDeletes(api huma.API) {
	op := rawAPIV1Operation("deleteLoopTarget", http.MethodDelete, "/loops/{target}/{action}", "Delete a loop control")
	handler := api.Middlewares().Handler(op.Middlewares.Handler(func(ctx huma.Context) {
		r, w := humago.Unwrap(ctx)
		target, action := r.PathValue("target"), r.PathValue("action")
		switch {
		case target == "suppressions":
			r.SetPathValue("id", action)
			s.loopHandler("deleteLoopSuppression")(w, r)
		case action == "snooze":
			r.SetPathValue("id", target)
			s.loopHandler("unsnoozeOpenLoop")(w, r)
		default:
			writeError(w, http.StatusNotFound, "not_found", "Open-loop route not found")
		}
	}))
	api.Adapter().Handle(&op, handler)
}

func loopOperation(id string) bool {
	switch id {
	case "listOpenLoops", "getWaitingLoops", "getOpenLoop", "closeOpenLoop", "snoozeOpenLoop", "unsnoozeOpenLoop", "listLoopSuppressions", "createLoopSuppression", "deleteLoopSuppression", "getLoopsStatus", "runLoops", "rebuildLoops", "listPersonLoops":
		return true
	default:
		return false
	}
}

func loopRouteParameters(id string) []*huma.Param {
	var out []*huma.Param
	switch id {
	case "getOpenLoop", "closeOpenLoop", "snoozeOpenLoop", "unsnoozeOpenLoop", "deleteLoopSuppression", "listPersonLoops":
		p := pathIntegerParam("Positive loop, suppression or person ID")
		minimum := float64(1)
		p.Schema.Minimum = &minimum
		out = append(out, p)
	}
	integer := func(name, doc string, def, max int) *huma.Param {
		p := queryIntegerParam(name, doc)
		lo, hi := float64(1), float64(max)
		p.Schema.Minimum, p.Schema.Maximum, p.Schema.Default = &lo, &hi, def
		return p
	}
	switch id {
	case "listOpenLoops":
		kind := queryStringParam("kind", "Loop kind", false)
		kind.Schema.Enum = []any{loops.ReplyOwed, loops.AwaitingReply, "commitment_owed_by_me", "commitment_owed_to_me"}
		status := queryStringParam("status", "Loop status", false)
		status.Schema.Enum, status.Schema.Default = []any{"open", "held", "closed", "all"}, "open"
		cursor := queryStringParam("cursor", "Opaque next cursor", false)
		maxLen := 512
		cursor.Schema.MaxLength = &maxLen
		out = append(out, kind, status, queryIntegerParam("person_id", "Filter by person"), queryIntegerParam("source_id", "Filter by archive source"), queryBooleanParam("include_snoozed", "Include currently snoozed loops"), integer("limit", "Maximum rows", 50, 200), cursor)
	case "getWaitingLoops":
		asOf := queryStringParam("as_of", "RFC3339 evaluation time; snooze eligibility remains current", false)
		asOf.Schema.Format = "date-time"
		out = append(out, integer("top", "Maximum counterpart groups", 10, 50), queryBooleanParam("allow_stale", "Return a stale ranking with warnings"), asOf)
	case "listPersonLoops":
		out = append(out, integer("limit", "Maximum open loops", 50, 200))
	}
	return out
}

func (s *Server) loopCapabilitiesEnabled() bool {
	return s.loopService != nil && s.loopService.Enabled()
}

// Loop routes are registered even when disabled, so clients get an explicit
// unavailable response. Discovery advertises them only when wired and enabled.
func filterLoopDescriptors(routes []apiprotocol.MCPRouteDescriptor, enabled bool) []apiprotocol.MCPRouteDescriptor {
	if enabled {
		return routes
	}
	out := make([]apiprotocol.MCPRouteDescriptor, 0, len(routes))
	for _, route := range routes {
		if !loopOperation(route.OperationID) {
			out = append(out, route)
		}
	}
	return out
}
