package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

type FederationStore interface {
	CreateFederationEnrollmentContext(ctx context.Context, spokeUID, member, name string) (*store.FederationEnrollment, string, error)
	ListFederationEnrollmentsContext(ctx context.Context) ([]store.FederationEnrollment, error)
	RevokeFederationEnrollmentContext(ctx context.Context, uid string) error
	AuthenticateFederationEnrollmentContext(ctx context.Context, token, spokeUID string) (*store.FederationEnrollment, error)
}

type federationCreateRequest struct {
	SpokeInstance string `json:"spoke_instance"`
	Member        string `json:"member"`
	SpokeName     string `json:"spoke_name,omitempty"`
}

type federationCreateResponse struct {
	Enrollment  store.FederationEnrollment `json:"enrollment"`
	Token       string                     `json:"token"`
	HubInstance string                     `json:"hub_instance"`
}

type federationListResponse struct {
	Enrollments []store.FederationEnrollment `json:"enrollments"`
}

type federationIdentityResponse struct {
	ArchiveUID string `json:"archive_uid"`
}

type federationHandshakeResponse struct {
	ProtocolVersions []int  `json:"protocol_versions"`
	HubInstance      string `json:"hub_instance"`
	EnrollmentState  string `json:"enrollment_state"`
}

func (s *Server) registerFederationRoutes(api huma.API) {
	op := rawAPIV1Operation("getFederationHandshake", http.MethodGet, "/federation/handshake", "Identify the hub for an enrolled spoke")
	op.Security = []map[string][]string{{federationTokenSecurityScheme: {}, federationSpokeSecurityScheme: {}}}
	op.Responses = jsonResponsesFor[federationHandshakeResponse](api)
	registerRawHumaRoute(api, op, s.handleFederationHandshake)
	registerAPIV1RawHumaJSONRoute[federationIdentityResponse](api, "getFederationIdentity", http.MethodGet, "/federation/identity", "Get this archive's federation identity", s.handleFederationIdentity)
	registerAPIV1RawHumaJSONRouteWithRequest[federationCreateRequest, federationCreateResponse](api, "createFederationEnrollment", http.MethodPost, "/federation/enrollments", "Enroll a spoke", s.handleFederationCreate, http.StatusCreated)
	registerAPIV1RawHumaJSONRoute[federationListResponse](api, "listFederationEnrollments", http.MethodGet, "/federation/enrollments", "List spoke enrollments", s.handleFederationList)
	op = rawAPIV1Operation("revokeFederationEnrollment", http.MethodDelete, "/federation/enrollments/{uid}", "Revoke a spoke enrollment")
	op.Parameters = []*huma.Param{{Name: "uid", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}}}
	op.Responses = rawHumaResponses(http.StatusNoContent)
	registerRawHumaRoute(api, op, s.handleFederationRevoke)
}

func (s *Server) federationRole(w http.ResponseWriter, r *http.Request, identity bool) bool {
	role := s.cfg.Federation.Role
	if role != "hub" && (!identity || role != "spoke") {
		s.handleNotFound(w, r)
		return false
	}
	return true
}

func (s *Server) federationOwner(w http.ResponseWriter, r *http.Request, identity bool) bool {
	mode := s.requestAuthentication(r).Mode
	if !s.ownerAccessAuthorized(r) && (!identity || mode != AuthModeLoopback || !isLoopbackRequest(r)) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
		return false
	}
	return true
}

func (s *Server) federationArchiveUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := s.store.(ArchiveIdentifier)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "Archive identity is unavailable")
		return "", false
	}
	uid, err := id.ArchiveUIDContext(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Archive identity is unavailable")
		return "", false
	}
	return uid, true
}

func (s *Server) handleFederationIdentity(w http.ResponseWriter, r *http.Request) {
	if !s.federationRole(w, r, true) || !s.federationOwner(w, r, true) {
		return
	}
	uid, ok := s.federationArchiveUID(w, r)
	if ok {
		writeJSON(w, http.StatusOK, federationIdentityResponse{ArchiveUID: uid})
	}
}

func (s *Server) handleFederationHandshake(w http.ResponseWriter, r *http.Request) {
	if !s.federationRole(w, r, false) {
		return
	}
	auth := s.requestAuthentication(r)
	if auth.Mode != AuthModeFederation && auth.Mode != AuthModeCaller {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
		return
	}
	uid, ok := s.federationArchiveUID(w, r)
	if !ok {
		return
	}
	// An empty state means the library callback admitted the request without checking an enrollment.
	state := ""
	if auth.Mode == AuthModeFederation {
		state = "active"
	}
	writeJSON(w, http.StatusOK, federationHandshakeResponse{ProtocolVersions: []int{1}, HubInstance: uid, EnrollmentState: state})
}

func (s *Server) federationManagement(w http.ResponseWriter, r *http.Request) bool {
	if !s.federationRole(w, r, false) || !s.federationOwner(w, r, false) {
		return false
	}
	if s.federationStore == nil {
		writeError(w, http.StatusServiceUnavailable, "federation_unavailable", "Federation store is unavailable")
		return false
	}
	return true
}

func (s *Server) handleFederationCreate(w http.ResponseWriter, r *http.Request) {
	if !s.federationManagement(w, r) {
		return
	}
	var req federationCreateRequest
	dec := jsontext.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := json.UnmarshalDecode(dec, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid enrollment request")
		return
	}
	if !requireSingleJSONValue(w, dec, "invalid_request") {
		return
	}
	uid, ok := s.federationArchiveUID(w, r)
	if !ok {
		return
	}
	e, token, err := s.federationStore.CreateFederationEnrollmentContext(r.Context(), req.SpokeInstance, req.Member, req.SpokeName)
	if err != nil {
		if errors.Is(err, store.ErrFederationValidation) {
			message := strings.TrimSuffix(err.Error(), ": "+store.ErrFederationValidation.Error())
			writeError(w, http.StatusBadRequest, "invalid_request", message)
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", "Could not create enrollment")
		}
		return
	}
	writeJSON(w, http.StatusCreated, federationCreateResponse{Enrollment: *e, Token: token, HubInstance: uid})
}

func (s *Server) handleFederationList(w http.ResponseWriter, r *http.Request) {
	if !s.federationManagement(w, r) {
		return
	}
	list, err := s.federationStore.ListFederationEnrollmentsContext(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not list enrollments")
		return
	}
	writeJSON(w, http.StatusOK, federationListResponse{Enrollments: list})
}

func (s *Server) handleFederationRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.federationManagement(w, r) {
		return
	}
	err := s.federationStore.RevokeFederationEnrollmentContext(r.Context(), r.PathValue("uid"))
	if errors.Is(err, store.ErrNotFound) {
		s.handleNotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not revoke enrollment")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
