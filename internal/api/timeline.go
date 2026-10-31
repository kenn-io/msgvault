package api

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/timeline"
)

type timelineSourceProvider interface {
	TimelineSources() (timeline.Sources, error)
}

// TimelineAmbiguityResponse lets a caller choose a person without guessing.
type TimelineAmbiguityResponse struct {
	ErrorResponse
	Candidates []timeline.Candidate `json:"candidates"`
}

func (s *Server) registerTimelineRoute(api huma.API) {
	op := rawAPIV1Operation("getTimeline", http.MethodGet, "/timeline", "Read bounded activity by day, person, or anchor")
	op.Tags = []string{"Timeline"}
	op.Parameters = timelineParameters()
	op.Responses = jsonResponsesFor[timeline.Timeline](api)
	op.Responses["200"].Content["text/markdown"] = &huma.MediaType{Schema: &huma.Schema{Type: huma.TypeString}}
	op.Errors = []int{400, 401, 403, 409, 500, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	op.Responses["409"].Content = jsonResponsesFor[TimelineAmbiguityResponse](api)["200"].Content
	registerRawHumaRoute(api, op, s.handleTimeline)
}

func timelineParameters() []*huma.Param {
	params := []*huma.Param{
		queryStringParam("mode", "View: day, relationship, or anchor; inferred from with or around when omitted", false),
		queryStringParam("date", "One local date (YYYY-MM-DD)", false),
		queryStringParam("from", "First local date, inclusive; requires to", false),
		queryStringParam("to", "Last local date, inclusive; requires from", false),
		queryIntegerParam("days", "Days ending today (1–31); defaults to two without other date bounds"),
		queryStringParam("with", "Person, participant, email, phone, or name reference", false),
		queryStringParam("around", "Message, conversation, event, burst, date, or datetime reference", false),
		queryIntegerParam("before", "Anchor neighbors before the target (0–50); default five"),
		queryIntegerParam("after", "Anchor neighbors after the target (0–50); default five"),
		queryBooleanParam("conversation", "Show individual messages in the anchor conversation"),
		queryStringParam("tiers", "Comma-separated person tiers (1, 2, or 3)", false),
		queryBooleanParam("include_cold", "Include archived cold activity"),
		queryBooleanParam("include_noise", "Include activity excluded as noise"),
		queryBooleanParam("brief", "Use shorter sections and a default budget of 6144 bytes"),
		queryIntegerParam("budget", "Final encoded bytes (2048–65536); default 16384"),
		queryStringParam("format", "Response encoding: json (default) or markdown", false),
		queryStringParam("cursor", "Continue the same request and authorization scope", false),
	}
	return params
}

func parseTimelineRequest(rawQuery string) (timeline.Request, error) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return timeline.Request{}, timeline.ErrInvalidRequest
	}
	for _, p := range timelineParameters() {
		if len(q[p.Name]) > 1 {
			return timeline.Request{}, timeline.ErrInvalidRequest
		}
	}
	req := timeline.Request{Mode: timeline.Mode(q.Get("mode")), Date: q.Get("date"), From: q.Get("from"), To: q.Get("to"), With: q.Get("with"), Around: q.Get("around"), Format: q.Get("format"), Cursor: q.Get("cursor")}
	req.BeforeSet, req.AfterSet = q.Has("before"), q.Has("after")
	if req.Format == "" {
		req.Format = "json"
	}
	for name, dst := range map[string]*int{"days": &req.Days, "before": &req.Before, "after": &req.After, "budget": &req.Budget} {
		if q.Has(name) {
			*dst, err = strconv.Atoi(q.Get(name))
			if err != nil {
				return req, timeline.ErrInvalidRequest
			}
			if (name == "days" || name == "budget") && *dst == 0 {
				return req, timeline.ErrInvalidRequest
			}
		}
	}
	for name, dst := range map[string]*bool{"conversation": &req.Conversation, "include_cold": &req.IncludeCold, "include_noise": &req.IncludeNoise, "brief": &req.Brief} {
		if q.Has(name) {
			*dst, err = strconv.ParseBool(q.Get(name))
			if err != nil {
				return req, timeline.ErrInvalidRequest
			}
		}
	}
	if q.Has("tiers") {
		for _, value := range strings.Split(q.Get("tiers"), ",") {
			tier, err := strconv.Atoi(value)
			if err != nil {
				return req, timeline.ErrInvalidRequest
			}
			req.Tiers = append(req.Tiers, tier)
		}
	}
	return req, nil
}

func (s *Server) timelineSources() (timeline.Sources, error) {
	if provider, ok := s.store.(timelineSourceProvider); ok {
		return provider.TimelineSources()
	}
	if st, ok := s.store.(*store.Store); ok {
		return timeline.NewStoreSources(st, s.cfg)
	}
	return timeline.Sources{}, errors.New("timeline readers unavailable")
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := parseTimelineRequest(r.URL.RawQuery)
	if err != nil {
		s.writeTimelineError(w, err)
		return
	}
	grant := s.requestAuthentication(r).Grant
	req.Scope.Owner = grant == nil
	if grant != nil {
		if req.With != "" {
			writeAPIHTTPError(w, agentReadDenied(agentgrant.PermissionMessageRead))
			return
		}
		ids := agentReadSourceIDs(r)
		admitted := false
		for _, id := range ids {
			admitted = admitted || id > 0
		}
		if !admitted {
			writeAPIHTTPError(w, agentReadDenied(agentgrant.PermissionMessageRead))
			return
		}
		ctx = sourcescope.WithSourceIDs(ctx, ids)
		req.Scope.OpenTools = timelineOpenTools(*grant)
	}
	src, err := s.timelineSources()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "timeline_unavailable", "Timeline readers are unavailable")
		return
	}
	src.Clock = s.clockNow
	src.Agenda = s.personAgendaOperations
	result, err := timeline.Build(ctx, src, req)
	if err != nil {
		s.writeTimelineError(w, err)
		return
	}
	var data []byte
	contentType := "application/json; charset=utf-8"
	if req.Format == "markdown" {
		data = []byte(timeline.Markdown(result))
		contentType = "text/markdown; charset=utf-8"
	} else {
		// Match Fit's compact wire encoding and stabilize all timeline maps.
		data, err = marshalAPIJSONBytes(result, json.Deterministic(true))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to encode timeline")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func timelineOpenTools(grant agentgrant.Grant) []string {
	tools := []string{}
	for _, name := range []string{"get_message", "list_messages", "get_calendar_event", "get_meeting_context"} {
		for _, permission := range agentgrant.ReadToolPermissions(name) {
			if grant.HasPermission(permission) {
				tools = append(tools, name)
				break
			}
		}
	}
	return tools
}

func (s *Server) writeTimelineError(w http.ResponseWriter, err error) {
	var ambiguous *timeline.ErrAmbiguousReference
	switch {
	case errors.As(err, &ambiguous):
		writeJSON(w, http.StatusConflict, TimelineAmbiguityResponse{ErrorResponse: ErrorResponse{Error: "ambiguous_reference", Message: "Choose a person from candidates"}, Candidates: ambiguous.Candidates})
	case errors.Is(err, timeline.ErrCursorExpired):
		writeError(w, http.StatusBadRequest, "cursor_expired", "Timeline continuation expired; retry the original request")
	case errors.Is(err, timeline.ErrInvalidRequest), errors.Is(err, timeline.ErrWindowTooLarge), errors.Is(err, timeline.ErrBudgetTooSmall), errors.Is(err, timeline.ErrUnknownReference), errors.Is(err, timeline.ErrUndatedAnchor):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to build timeline")
	}
}
