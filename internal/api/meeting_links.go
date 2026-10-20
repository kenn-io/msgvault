package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/store"
)

type MeetingLinkStore interface {
	MeetingLinksContext(ctx context.Context, messageID int64) ([]store.MeetingLink, error)
	LinkMeetingContext(ctx context.Context, memberID int64, ref store.OccurrenceRef) error
	UnlinkMeetingContext(ctx context.Context, memberID int64, ref store.OccurrenceRef) error
	SuggestMeetingLinksContext(ctx context.Context, ref store.MeetingLinkRef, limit int) ([]store.MeetingSuggestion, error)
}

type MeetingLinkDecisionRequest struct {
	MessageID int64  `json:"message_id" minimum:"1" maximum:"9007199254740991"`
	Event     string `json:"event"`
	Decision  string `json:"decision" enum:"link,unlink"`
}

type MeetingLinkDecisionResponse struct {
	MessageID int64  `json:"message_id"`
	Event     string `json:"event"`
	Decision  string `json:"decision"`
}

type MeetingSuggestionsResponse struct {
	Suggestions []store.MeetingSuggestion `json:"suggestions"`
}

type MeetingLinksResponse struct {
	Links []store.MeetingLink `json:"links"`
}

func (s *Server) registerMeetingLinkRoutes(api huma.API) {
	links := rawAPIV1Operation("getMeetingLinks", http.MethodGet, "/meetings/links", "List a note's linked calendar occurrences")
	links.Tags = []string{"Meetings"}
	links.Parameters = []*huma.Param{{Name: "message_id", In: "query", Required: true, Schema: &huma.Schema{Type: "integer", Format: "int64", Minimum: new(float64(1)), Maximum: new(float64(9007199254740991))}}}
	links.Responses = jsonResponsesFor[MeetingLinksResponse](api)
	links.Errors = []int{400, 401, 403, 404, 500, 503}
	addErrorResponses(api, links.Responses, links.Errors...)
	registerRawHumaRoute(api, links, s.handleMeetingLinks)

	registerMeetingRoute[MeetingLinkDecisionRequest, MeetingLinkDecisionResponse](api, "decideMeetingLink", "/meetings/links/decisions", "Link or unlink an archived meeting", s.handleMeetingLinkDecision)
	op := rawAPIV1Operation("suggestMeetingLinks", http.MethodGet, "/meetings/suggestions", "Suggest archived meeting links")
	op.Tags = []string{"Meetings"}
	op.Parameters = []*huma.Param{{Name: "ref", In: "query", Required: true, Schema: &huma.Schema{Type: "string"}}}
	op.Responses = jsonResponsesFor[MeetingSuggestionsResponse](api)
	op.Errors = []int{400, 401, 403, 404, 500, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleMeetingLinkSuggestions)
}

func parseMeetingLinkRef(value string, eventOnly bool) (store.MeetingLinkRef, error) {
	kind, value, ok := strings.Cut(value, ":")
	if !ok || kind != "evt" && (eventOnly || kind != "msg") {
		return store.MeetingLinkRef{}, errors.New("use msg:<id> or evt:<id>[@occurrence key]; decisions require an event reference")
	}
	message, key, hasKey := strings.Cut(value, "@")
	id, err := strconv.ParseInt(message, 10, 64)
	if err != nil || id <= 0 || id > 9007199254740991 || hasKey && (kind != "evt" || key == "") {
		return store.MeetingLinkRef{}, errors.New("invalid meeting reference")
	}
	if hasKey {
		if _, identity, ok := calrecur.ParseInstanceID("event_" + key); !ok || identity.Key() != key {
			return store.MeetingLinkRef{}, errors.New("invalid occurrence key")
		}
	}
	return store.MeetingLinkRef{Kind: kind, MessageID: id, Key: key}, nil
}

func (s *Server) handleMeetingLinkDecision(w http.ResponseWriter, r *http.Request) {
	var request MeetingLinkDecisionRequest
	if _, ok := decodeMeetingRequest(w, r, &request); !ok {
		return
	}
	ref, err := parseMeetingLinkRef(request.Event, true)
	if err != nil || request.MessageID <= 0 || request.MessageID > 9007199254740991 || request.Decision != "link" && request.Decision != "unlink" {
		writeError(w, 400, "invalid_argument", "message_id, an evt: reference, and decision link or unlink are required")
		return
	}
	backend, ok := s.store.(MeetingLinkStore)
	if !ok {
		writeError(w, 503, "meetings_unavailable", "Meeting links are unavailable")
		return
	}
	if request.Decision == "link" {
		err = backend.LinkMeetingContext(r.Context(), request.MessageID, ref.OccurrenceRef)
	} else {
		err = backend.UnlinkMeetingContext(r.Context(), request.MessageID, ref.OccurrenceRef)
	}
	if err != nil {
		s.writeMeetingLinkError(w, err)
		return
	}
	writeJSON(w, 200, MeetingLinkDecisionResponse(request))
}

func (s *Server) handleMeetingLinkSuggestions(w http.ResponseWriter, r *http.Request) {
	ref, err := parseMeetingLinkRef(r.URL.Query().Get("ref"), false)
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	backend, ok := s.store.(MeetingLinkStore)
	if !ok {
		writeError(w, 503, "meetings_unavailable", "Meeting links are unavailable")
		return
	}
	suggestions, err := backend.SuggestMeetingLinksContext(r.Context(), ref, 10)
	if err != nil {
		s.writeMeetingLinkError(w, err)
		return
	}
	if suggestions == nil {
		suggestions = []store.MeetingSuggestion{}
	}
	writeJSON(w, 200, MeetingSuggestionsResponse{Suggestions: suggestions})
}

func (s *Server) writeMeetingLinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, 404, "meeting_not_found", "Meeting or event occurrence not found")
	case errors.Is(err, store.ErrQueryTooBroad), errors.Is(err, calrecur.ErrBudgetExceeded):
		writeError(w, 400, "query_too_broad", err.Error())
	case errors.Is(err, calrecur.ErrUnsupportedRecurrence):
		writeError(w, 400, "invalid_occurrence", err.Error())
	default:
		if !s.writeIfContextError(w, err) {
			writeError(w, 500, "meeting_links_failed", err.Error())
		}
	}
}

func (s *Server) handleMeetingLinks(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("message_id"), 10, 64)
	if err != nil || id <= 0 || id > 9007199254740991 {
		writeError(w, 400, "invalid_argument", "message_id must be a positive message ID")
		return
	}
	backend, ok := s.store.(MeetingLinkStore)
	if !ok {
		writeError(w, 503, "meetings_unavailable", "Meeting links are unavailable")
		return
	}
	links, err := backend.MeetingLinksContext(r.Context(), id)
	if err != nil {
		s.writeMeetingLinkError(w, err)
		return
	}
	if links == nil {
		links = []store.MeetingLink{}
	}
	writeJSON(w, 200, MeetingLinksResponse{Links: links})
}
