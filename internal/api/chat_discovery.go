package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

type ChatDiscoveryStore interface {
	SearchChatsContext(ctx context.Context, q store.ChatDiscoveryQuery) (*store.ChatDiscoveryPage, error)
}

func (s *Server) handleSearchChats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.rejectBadParam(w, newParamError("query", "chat discovery query is malformed"))
		return
	}
	for _, key := range []string{"limit", "source_id"} {
		if values.Has(key) && strings.TrimSpace(values.Get(key)) == "" {
			writeError(w, http.StatusBadRequest, "invalid_query", key+" must not be empty")
			return
		}
	}
	limit, present, err := queryInt(r, "limit")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if present && limit == 0 {
		writeError(w, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 100")
		return
	}
	sourceID, present, err := queryInt64(r, "source_id")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if present && sourceID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_query", "source_id must be positive")
		return
	}
	q := store.ChatDiscoveryQuery{Query: values.Get("q"), Limit: limit, SourceID: sourceID}
	reader, ok := s.store.(ChatDiscoveryStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "chat_discovery_unavailable", "Chat discovery is unavailable")
		return
	}
	page, err := reader.SearchChatsContext(r.Context(), q)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInvalidChatDiscoveryQuery) {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		s.logger.Error("discover archived chats", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not discover archived chats")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
