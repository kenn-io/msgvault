package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector/visual"
)

type visualTextSearchRequest struct {
	Text           string `json:"text"`
	Limit          int    `json:"limit,omitempty"`
	SenderPersonID int64  `json:"sender_person_id,omitempty"`
	SourceID       int64  `json:"source_id,omitempty"`
	MessageID      int64  `json:"message_id,omitempty"`
	Filename       string `json:"filename,omitempty"`
	MIMEPrefix     string `json:"mime_prefix,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
	After          string `json:"after,omitempty"`
	Before         string `json:"before,omitempty"`
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
		if raw := r.FormValue("sender_person_id"); raw != "" {
			query.SenderPersonID, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || query.SenderPersonID < 1 {
				writeError(w, http.StatusBadRequest, "invalid_sender_person_id", "sender_person_id must be positive")
				return
			}
		}
		for raw, target := range map[string]*int64{
			"source_id": &query.SourceID, "message_id": &query.MessageID,
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
		query.Text, query.Limit, query.SenderPersonID = request.Text, request.Limit, request.SenderPersonID
		query.SourceID, query.MessageID = request.SourceID, request.MessageID
		query.Filename, query.MIMEPrefix, query.Cursor = request.Filename, request.MIMEPrefix, request.Cursor
		if query.SenderPersonID < 0 || query.SourceID < 0 || query.MessageID < 0 {
			writeError(w, http.StatusBadRequest, "invalid_visual_filter", "ID filters must be positive")
			return
		}
		if !parseVisualSearchDates(w, request.After, request.Before, &query) {
			return
		}
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

func parseVisualSearchDates(w http.ResponseWriter, after, before string, query *visual.SearchQuery) bool {
	var err error
	if after != "" {
		parsed, parseErr := time.Parse("2006-01-02", after)
		err = parseErr
		query.After = &parsed
	}
	if err == nil && before != "" {
		parsed, parseErr := time.Parse("2006-01-02", before)
		err = parseErr
		query.Before = &parsed
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_visual_date", "after and before must use YYYY-MM-DD")
		return false
	}
	if query.After != nil && query.Before != nil && !query.After.Before(*query.Before) {
		writeError(w, http.StatusBadRequest, "invalid_visual_date_range", "after must be before before")
		return false
	}
	return true
}
