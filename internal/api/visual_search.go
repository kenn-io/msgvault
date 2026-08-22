package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/vector/visual"
)

type visualTextSearchRequest struct {
	Text           string                  `json:"text"`
	Limit          int                     `json:"limit,omitempty"`
	SenderPersonID int64                   `json:"sender_person_id,omitempty"`
	PersonID       int64                   `json:"person_id,omitempty"`
	ParticipantID  int64                   `json:"participant_id,omitempty"`
	Directions     []personscope.Direction `json:"directions,omitempty"`
	SourceID       int64                   `json:"source_id,omitempty"`
	MessageID      int64                   `json:"message_id,omitempty"`
	Filename       string                  `json:"filename,omitempty"`
	MIMEPrefix     string                  `json:"mime_prefix,omitempty"`
	Cursor         string                  `json:"cursor,omitempty"`
	After          string                  `json:"after,omitempty"`
	Before         string                  `json:"before,omitempty"`
}

func (s *Server) handleVisualSearch(w http.ResponseWriter, r *http.Request) {
	s.vectorMu.RLock()
	service := s.visualSearch
	s.vectorMu.RUnlock()
	if service == nil {
		writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", "Visual attachment search is not ready")
		return
	}
	query := visual.SearchQuery{}
	var personID, participantID, senderPersonID int64
	var directions []personscope.Direction
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "multipart/") {
		r.Body = http.MaxBytesReader(w, r.Body, visual.MaxQueryImageBytes+(1<<20))
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_image_query", "Invalid image query upload")
			return
		}
		file, _, err := r.FormFile("image")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_image_query", "Image form field is required")
			return
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(io.LimitReader(file, visual.MaxQueryImageBytes+1))
		if err != nil || int64(len(data)) > visual.MaxQueryImageBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "image_query_too_large", "Image query exceeds the upload limit")
			return
		}
		query.Image, err = visual.DecodeQueryImage(data)
		if err != nil {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_image_query", err.Error())
			return
		}
		if raw := r.FormValue("limit"); raw != "" {
			query.Limit, err = strconv.Atoi(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer")
				return
			}
		}
		for raw, target := range map[string]*int64{
			"source_id": &query.SourceID, "message_id": &query.MessageID,
			"person_id": &personID, "participant_id": &participantID,
			"sender_person_id": &senderPersonID,
		} {
			value := r.FormValue(raw)
			if value == "" {
				continue
			}
			parsed, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || parsed < 1 {
				writeError(w, http.StatusBadRequest, "invalid_"+raw, raw+" must be positive")
				return
			}
			*target = parsed
		}
		for _, raw := range r.MultipartForm.Value["direction"] {
			for value := range strings.SplitSeq(raw, ",") {
				directions = append(directions, personscope.Direction(strings.TrimSpace(value)))
			}
		}
		query.Filename, query.MIMEPrefix, query.Cursor = r.FormValue("filename"), r.FormValue("mime_prefix"), r.FormValue("cursor")
		if !parseVisualSearchDates(w, r.FormValue("after"), r.FormValue("before"), &query) {
			return
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var request visualTextSearchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_visual_query", "Invalid visual search request")
			return
		}
		query.Text, query.Limit = request.Text, request.Limit
		personID, participantID, senderPersonID = request.PersonID, request.ParticipantID, request.SenderPersonID
		directions = request.Directions
		query.SourceID, query.MessageID = request.SourceID, request.MessageID
		query.Filename, query.MIMEPrefix, query.Cursor = request.Filename, request.MIMEPrefix, request.Cursor
		if personID < 0 || participantID < 0 || senderPersonID < 0 || query.SourceID < 0 || query.MessageID < 0 {
			writeError(w, http.StatusBadRequest, "invalid_visual_filter", "ID filters must be positive")
			return
		}
		if !parseVisualSearchDates(w, request.After, request.Before, &query) {
			return
		}
	}
	if !s.resolveVisualPersonScope(w, r, &query, personID, participantID, senderPersonID, directions) {
		return
	}
	response, err := service.Search(r.Context(), query)
	if err != nil {
		switch {
		case errors.Is(err, visual.ErrInvalidQuery):
			writeError(w, http.StatusBadRequest, "invalid_visual_query", err.Error())
		case errors.Is(err, visual.ErrSearchNotReady):
			writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", err.Error())
		case errors.Is(err, visual.ErrInvalidCursor):
			writeError(w, http.StatusConflict, "visual_cursor_changed", err.Error())
		default:
			writeError(w, http.StatusBadGateway, "visual_search_failed", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) resolveVisualPersonScope(
	w http.ResponseWriter,
	r *http.Request,
	query *visual.SearchQuery,
	personID, participantID, senderPersonID int64,
	directions []personscope.Direction,
) bool {
	if senderPersonID > 0 {
		if personID > 0 || participantID > 0 || len(directions) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_visual_person_scope",
				"sender_person_id cannot be combined with person_id, participant_id, or directions")
			return false
		}
		personID = senderPersonID
		directions = []personscope.Direction{personscope.FromPerson}
	}
	if personID > 0 && participantID > 0 {
		writeError(w, http.StatusBadRequest, "invalid_visual_person_scope",
			"person_id and participant_id are mutually exclusive")
		return false
	}
	if len(directions) > 0 && personID == 0 && participantID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_visual_person_scope",
			"directions require person_id or participant_id")
		return false
	}
	if _, _, err := resolver.NormalizeDirections(directions); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_visual_person_scope", err.Error())
		return false
	}
	if personID == 0 && participantID == 0 {
		return true
	}
	reference := resolver.Reference{Kind: resolver.ReferencePerson, ID: personID}
	if participantID > 0 {
		reference = resolver.Reference{Kind: resolver.ReferenceParticipant, ID: participantID}
	}
	resolved, err := resolver.Resolve(r.Context(), s.store, reference, directions)
	if err != nil {
		s.writePersonScopeError(w, reference, err, "visual")
		return false
	}
	query.Person = &resolved.Scope
	return true
}

func parseVisualSearchDates(w http.ResponseWriter, after, before string, query *visual.SearchQuery) bool {
	var err error
	if after != "" {
		parsed, parseErr := parseAPITime(after)
		err = parseErr
		query.After = &parsed
	}
	if err == nil && before != "" {
		parsed, parseErr := parseAPITime(before)
		err = parseErr
		query.Before = &parsed
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_visual_date",
			"after and before must use RFC3339 or YYYY-MM-DD")
		return false
	}
	if query.After != nil && query.Before != nil && !query.After.Before(*query.Before) {
		writeError(w, http.StatusBadRequest, "invalid_visual_date_range", "after must be before before")
		return false
	}
	return true
}
