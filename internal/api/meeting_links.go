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
	MeetingRecordContext(ctx context.Context, ref store.MeetingLinkRef) (store.MeetingRecord, error)
	LinkMeetingContext(ctx context.Context, memberID int64, ref store.OccurrenceRef) error
	UnlinkMeetingContext(ctx context.Context, memberID int64, ref store.OccurrenceRef) error
	SuggestMeetingLinksContext(ctx context.Context, ref store.MeetingLinkRef, limit int) (store.MeetingSuggestionPage, error)
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
	Suggestions  []store.MeetingSuggestion    `json:"suggestions"`
	Unexpandable []store.CalendarUnexpandable `json:"unexpandable"`
}

func (s *Server) registerMeetingLinkRoutes(api huma.API) {
	links := rawAPIV1Operation("getMeetingRecord", http.MethodGet, "/meetings/record", "Read an archived meeting from a note or event")
	links.Tags = []string{"Meetings"}
	links.Parameters = []*huma.Param{{Name: "ref", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
	links.Responses = jsonResponsesFor[store.MeetingRecord](api)
	links.Errors = []int{400, 401, 403, 404, 500, 503}
	addErrorResponses(api, links.Responses, links.Errors...)
	registerRawHumaRoute(api, links, s.handleMeetingRecord)

	registerMeetingRoute[MeetingLinkDecisionRequest, MeetingLinkDecisionResponse](api, "decideMeetingLink", "/meetings/links/decisions", "Link or unlink an archived meeting", s.handleMeetingLinkDecision)
	op := rawAPIV1Operation("suggestMeetingLinks", http.MethodGet, "/meetings/suggestions", "Suggest archived meeting links")
	op.Tags = []string{"Meetings"}
	op.Parameters = []*huma.Param{{Name: "ref", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
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
		if base, _, ok := calrecur.ParseInstanceID("event_" + key); !ok || base != "event" {
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
	s.refreshIdentityCacheState(r.Context())
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
	if suggestions.Suggestions == nil {
		suggestions.Suggestions = []store.MeetingSuggestion{}
	}
	if suggestions.Unexpandable == nil {
		suggestions.Unexpandable = []store.CalendarUnexpandable{}
	}
	writeJSON(w, 200, MeetingSuggestionsResponse{Suggestions: suggestions.Suggestions, Unexpandable: suggestions.Unexpandable})
}

func (s *Server) writeMeetingLinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		message := "Meeting or event occurrence not found"
		if errors.Unwrap(err) != nil {
			message = strings.TrimSuffix(err.Error(), ": "+sql.ErrNoRows.Error())
		}
		writeError(w, 404, "meeting_not_found", message)
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

func (s *Server) handleMeetingRecord(w http.ResponseWriter, r *http.Request) {
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
	record, err := backend.MeetingRecordContext(r.Context(), ref)
	if err != nil {
		s.writeMeetingLinkError(w, err)
		return
	}
	writeJSON(w, 200, record)
}
