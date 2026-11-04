package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

type cardDAVParticipantBindingStore interface {
	ReconcileCardDAVParticipantBindingsContext(ctx context.Context, options store.CardDAVParticipantBindingOptions) (*store.CardDAVParticipantBindingResult, error)
}

func (s *Server) registerCardDAVParticipantBindingRoutes(api huma.API) {
	registerCardDAVJSONRouteWithRequest[store.CardDAVParticipantBindingOptions, store.CardDAVParticipantBindingResult](api,
		"reconcileCardDAVParticipantBindings", http.MethodPost, "/carddav/participant-bindings",
		"Preview, reconcile, or undo automatic CardDAV participant memberships", s.handleCardDAVParticipantBindings,
		http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable)
}

func (s *Server) handleCardDAVParticipantBindings(w http.ResponseWriter, r *http.Request) {
	bindings, ok := s.store.(cardDAVParticipantBindingStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV participant binding is unavailable")
		return
	}
	var options store.CardDAVParticipantBindingOptions
	if !decodeCardDAV(w, r, &options) {
		return
	}
	result, err := bindings.ReconcileCardDAVParticipantBindingsContext(r.Context(), options)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "carddav_binding_failed", "CardDAV participant binding failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}
