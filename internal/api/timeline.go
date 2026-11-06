package api

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/oauth"
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
		queryIntegerParam("budget", "Final encoded bytes (4096–65536); default 16384"),
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
		for value := range strings.SplitSeq(q.Get("tiers"), ",") {
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
	ctx := r.Context()
	req, err := parseTimelineRequest(r.URL.RawQuery)
	if err != nil {
		s.writeTimelineError(w, err)
		return
	}
	grant := s.requestAuthentication(r).Grant
	req.Scope.Owner = grant == nil
	if grant != nil {
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
	src.Freshness = s.timelineFreshness
	src.Clock, src.Agenda, src.Loops = s.clockNow, s.personAgendaOperations, s.timelineLoops(src.Loops)
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- Encoded JSON or Markdown is served as non-HTML with nosniff.
	_, _ = w.Write(data)
}

func timelineOpenTools(grant agentgrant.Grant) []string {
	tools := []string{}
	for _, name := range []string{"get_message", "list_messages", "get_calendar_event", "get_meeting_context"} {
		if slices.ContainsFunc(agentgrant.ReadToolPermissions(name), grant.HasPermission) {
			tools = append(tools, name)
		}
	}
	return tools
}

func (s *Server) writeTimelineError(w http.ResponseWriter, err error) {
	var ambiguous *timeline.ErrAmbiguousReference
	switch {
	case errors.As(err, &ambiguous):
		detail := ErrorResponse{Error: "ambiguous_reference", Message: "Choose a person from candidates"}
		writeJSON(w, http.StatusConflict, TimelineAmbiguityResponse{ErrorResponse: detail, Candidates: ambiguous.Candidates})
	case errors.Is(err, timeline.ErrCursorExpired):
		writeError(w, http.StatusBadRequest, "cursor_expired", "Timeline continuation expired; retry the original request")
	case errors.Is(err, timeline.ErrInvalidRequest), errors.Is(err, timeline.ErrWindowTooLarge), errors.Is(err, timeline.ErrBudgetTooSmall), errors.Is(err, timeline.ErrUnknownReference), errors.Is(err, timeline.ErrUndatedAnchor):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to build timeline")
	}
}

func (s *Server) timelineFreshness(ctx context.Context, now time.Time) (timeline.Freshness, error) {
	result := timeline.Freshness{AsOf: now, Sources: []timeline.SourceFreshness{}, Projections: []timeline.ProjectionFreshness{}, Warnings: []string{}}
	statusStore, ok := s.store.(SourceStatusStore)
	if !ok {
		return result, errors.New("source status unavailable")
	}
	sources, err := statusStore.ListSourcesContext(ctx, "")
	if err != nil {
		return result, err
	}
	for _, source := range sources {
		status, err := s.sourceStatus(ctx, statusStore, source, sources)
		if err != nil {
			return result, err
		}
		if !status.Scheduled {
			continue
		}
		item := timeline.SourceFreshness{SourceID: new(source.ID), SourceType: source.SourceType, Identifier: source.Identifier, Scheduling: "scheduled", Outcome: "never", SyncCommand: timelineSyncCommand(source)}
		if status.LatestSync != nil {
			item.Outcome = status.LatestSync.Status
			item.LastSyncErrors = status.LatestSync.ErrorsCount
		}
		if status.LastSuccessfulSync != nil && status.LastSuccessfulSync.CompletedAt != nil {
			at, err := time.Parse(time.RFC3339, *status.LastSuccessfulSync.CompletedAt)
			if err != nil {
				return result, fmt.Errorf("parse timeline last successful sync: %w", err)
			}
			item.LastSuccessfulSyncAt = &at
		}
		if item.LastSuccessfulSyncAt == nil {
			item.Reason = "never_synced"
		} else if status.Schedule != "" {
			schedule, err := cronexpr.Parser.Parse(status.Schedule)
			if err != nil {
				return result, fmt.Errorf("parse timeline sync schedule: %w", err)
			}
			if schedule.Next(*item.LastSuccessfulSyncAt).Before(now) {
				item.Reason = "stale"
			}
		}
		if item.Warn() {
			result.Sources = append(result.Sources, item)
		}
	}
	return result, nil
}

func timelineSyncCommand(item *store.Source) string {
	arg := shellArgument(item.Identifier)
	switch item.SourceType {
	case "gmail", "imap", "msmail":
		return "msgvault sync-incremental " + arg
	case "teams", "discord":
		return "msgvault sync-" + item.SourceType + " " + arg
	case "beeper", "matrix":
		command := "msgvault sync-" + item.SourceType
		if item.ID > 0 {
			command += " --account " + arg
		}
		return command
	case "slack":
		return "msgvault sync-slack"
	case "omi", "granola", "plaud", "circleback", "notion_meetings", "pocket", "inline", "twilio", "twenty", "muesli":
		return "msgvault sync-" + strings.ReplaceAll(item.SourceType, "_", "-") + " " + arg
	case "gcal":
		account, _, _ := strings.Cut(item.Identifier, "/")
		return "msgvault sync-calendar " + shellArgument(account)
	default:
		return ""
	}
}

func shellArgument(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"`$;&|<>()*?!#\\") && !strings.HasPrefix(value, "-") {
		return value
	}
	return oauth.ShellQuote(value)
}
