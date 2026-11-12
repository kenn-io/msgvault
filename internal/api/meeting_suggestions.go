package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/verification"
)

type MeetingSuggestionStore interface {
	SuggestMeetingLinksContext(context.Context, store.MeetingLinkRef, int, func(context.Context, []int64) (map[int64]bool, error)) (store.MeetingSuggestionPage, error)
}

func (s *Server) registerMeetingSuggestionRoute(api huma.API) {
	op := rawAPIV1Operation("suggestMeetingLinks", http.MethodGet, "/meetings/suggestions", "Suggest links between meeting notes and calendar events")
	op.Tags = []string{"Meetings"}
	limit := contactIntegerParam("limit", 1, 100)
	limit.Schema.Default = 10
	op.Parameters = []*huma.Param{{Name: "ref", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}, limit}
	op.Responses = jsonResponsesFor[store.MeetingSuggestionPage](api)
	op.Errors = []int{400, 401, 403, 404, 500, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleMeetingSuggestions)
}

func (s *Server) handleMeetingSuggestions(w http.ResponseWriter, r *http.Request) {
	ref, err := parseMeetingLinkRef(r.URL.Query().Get("ref"))
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, 400, "invalid_argument", "limit must be an integer between 1 and 100")
			return
		}
	}
	backend, ok := s.store.(MeetingSuggestionStore)
	if !ok {
		writeError(w, 503, "meetings_unavailable", "Meeting suggestions are unavailable")
		return
	}
	var classify func(context.Context, []int64) (map[int64]bool, error)
	if s.withholdsVerification(r) {
		classify = s.classifyVerification
	}
	page, err := backend.SuggestMeetingLinksContext(r.Context(), ref, limit, classify)
	if err != nil {
		if errors.Is(err, errVerificationUnavailable) {
			writeAPIHTTPError(w, newAPIHTTPError(503, "source_scope_unavailable", "Message content classification is unavailable"))
			return
		}
		s.writeMeetingLinkError(w, err)
		return
	}
	for i := range page.Suggestions {
		if page.Suggestions[i].TitleWithheld {
			page.Suggestions[i].Title = verification.Masked
		}
	}
	writeJSON(w, 200, page)
}
