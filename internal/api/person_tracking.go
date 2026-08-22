package api

import (
	"bytes"
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

// PersonTrackingStore is the feature-local person tracking capability.
type PersonTrackingStore interface {
	GetPersonTrackingContext(
		ctx context.Context, personID int64,
	) (*store.PersonTracking, error)
	SetPersonTrackingContext(
		ctx context.Context, personID int64, tracked bool,
	) (*store.PersonTracking, error)
}

var _ PersonTrackingStore = (*store.Store)(nil)

// PutPersonTrackingRequest replaces a person's explicit tracking state.
type PutPersonTrackingRequest struct {
	Tracked bool `json:"tracked"`
}

func (s *Server) registerPersonTrackingRoutes(api huma.API) {
	get := rawAPIV1Operation("getPersonTracking", http.MethodGet,
		"/people/{id}/tracking", "Get a person's tracking state")
	addPersonIDParameter(&get)
	get.Responses = jsonResponsesFor[store.PersonTracking](api)
	addErrorResponses(api, get.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetPersonTracking)

	set := rawAPIV1Operation("setPersonTracking", http.MethodPut,
		"/people/{id}/tracking", "Replace a person's tracking state")
	addPersonIDParameter(&set)
	set.RequestBody = jsonRequestBodyFor[PutPersonTrackingRequest](api)
	set.Responses = jsonResponsesFor[store.PersonTracking](api)
	addErrorResponses(api, set.Responses, http.StatusBadRequest,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, set, s.handleSetPersonTracking)
}

func (s *Server) handleGetPersonTracking(w http.ResponseWriter, r *http.Request) {
	tracking, ok := s.personTrackingStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	state, err := tracking.GetPersonTrackingContext(r.Context(), personID)
	if err != nil {
		s.writePersonError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleSetPersonTracking(w http.ResponseWriter, r *http.Request) {
	tracking, ok := s.personTrackingStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	var request PutPersonTrackingRequest
	fields, ok := decodePersonRequestFields(w, r, &request)
	if !ok {
		return
	}
	rawTracked, present := fields["tracked"]
	if !present {
		writeError(w, http.StatusBadRequest, "bad_request", "tracked is required")
		return
	}
	if bytes.Equal(bytes.TrimSpace(rawTracked), []byte("null")) {
		writeError(w, http.StatusBadRequest, "bad_request", "tracked must be a boolean")
		return
	}
	state, err := tracking.SetPersonTrackingContext(r.Context(), personID, request.Tracked)
	if err != nil {
		s.writePersonError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) personTrackingStore(w http.ResponseWriter) (PersonTrackingStore, bool) {
	tracking, ok := s.store.(PersonTrackingStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable,
			"tracking_unavailable", "Person tracking is unavailable")
	}
	return tracking, ok
}
