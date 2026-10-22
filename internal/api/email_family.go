package api

import (
	"context"
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"strconv"
	"strings"
)

// EmailFamilyStore is the archive-only read capability shared by family routes.
type EmailFamilyStore interface {
	EmailFamilyContext(ctx context.Context, messageID int64, options store.EmailFamilyOptions) (*store.EmailFamily, error)
}

func (s *Server) registerEmailFamilyRoutes(api huma.API) {
	op := rawAPIV1Operation("getEmailFamily", http.MethodGet, "/messages/{id}/family", "Read related archived email copies and explicit handling evidence")
	op.Parameters = []*huma.Param{pathIntegerParam("Live archived email message ID"), queryIntegerArrayParam("source_id", "Source IDs to include; repeat or comma-separate. Empty excludes all.")}
	op.Responses = jsonResponsesFor[store.EmailFamily](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleGetEmailFamily)
}

func emailFamilyOptions(r *http.Request) (store.EmailFamilyOptions, error) {
	var options store.EmailFamilyOptions
	if values, present := r.URL.Query()["source_id"]; present {
		options.SourceIDs = []int64{}
		for _, value := range values {
			if value == "" {
				continue
			}
			for token := range strings.SplitSeq(value, ",") {
				id, err := strconv.ParseInt(token, 10, 64)
				if err != nil || id < 1 {
					return options, errors.New("source_id must contain positive integers")
				}
				options.SourceIDs = append(options.SourceIDs, id)
			}
		}
		if len(options.SourceIDs) > 100 {
			return options, errors.New("at most 100 source IDs are allowed")
		}
	}
	return options, nil
}

func (s *Server) handleGetEmailFamily(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "invalid_message_id", "Message ID must be a positive integer")
		return
	}
	options, err := emailFamilyOptions(r)
	if err != nil {
		writeError(w, 400, "invalid_source_ids", err.Error())
		return
	}
	backend, ok := s.store.(EmailFamilyStore)
	if !ok {
		writeError(w, 503, "email_family_unavailable", "Email family lookup is unavailable")
		return
	}
	family, err := backend.EmailFamilyContext(r.Context(), id, options)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not_found", "Message not found")
		return
	}
	if err != nil {
		s.logger.Error("email family lookup failed", "error", err)
		writeError(w, 503, "email_family_unavailable", "Email family lookup is unavailable")
		return
	}
	writeJSON(w, 200, family)
}
