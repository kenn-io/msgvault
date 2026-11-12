package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/verification"
)

type MeetingSuggestionStore interface {
	SuggestMeetingLinksContext(context.Context, store.MeetingLinkRef, int) (store.MeetingSuggestionPage, error)
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
	page, err := backend.SuggestMeetingLinksContext(r.Context(), ref, limit)
	if err != nil {
		s.writeMeetingLinkError(w, err)
		return
	}
	ids := make([]int64, 0, len(page.Suggestions)*2)
	for _, item := range page.Suggestions {
		ids = append(ids, item.TitleMessageID)
		if item.TitleOriginID != 0 {
			ids = append(ids, item.TitleOriginID)
		}
	}
	withheld, ok := s.verificationWithheld(w, r, ids)
	if !ok {
		return
	}
	for i := range page.Suggestions {
		if withheld[page.Suggestions[i].TitleMessageID] || withheld[page.Suggestions[i].TitleOriginID] {
			page.Suggestions[i].Title = verification.Masked
		}
	}
	writeJSON(w, 200, page)
}
