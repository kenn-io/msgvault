package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

// EmailFamilyStore is the archive-only read behind the related-copies route.
type EmailFamilyStore interface {
	EmailFamilyContext(ctx context.Context, messageID int64) (*store.EmailFamily, error)
}

var _ EmailFamilyStore = (*store.Store)(nil)

func (s *Server) registerEmailFamilyRoutes(api huma.API) {
	op := rawAPIV1Operation("getEmailFamily", http.MethodGet, "/messages/{id}/family", "Read the other archived copies, replies and forwards of an email")
	op.Parameters = []*huma.Param{pathIntegerParam("Live archived email message ID")}
	op.Responses = jsonResponsesFor[store.EmailFamily](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleGetEmailFamily)
}

func (s *Server) handleGetEmailFamily(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_message_id", "Message ID must be a positive integer")
		return
	}
	backend, ok := s.store.(EmailFamilyStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "email_family_unavailable", "Email family lookup is unavailable")
		return
	}
	family, err := backend.EmailFamilyContext(r.Context(), id)
	if errors.Is(err, store.ErrMessageNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Message not found")
		return
	}
	if err != nil {
		s.logger.Error("email family lookup failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "email_family_unavailable", "Email family lookup is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, family)
}
